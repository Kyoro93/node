/*
 * Copyright (C) 2017 The "MysteriumNetwork/node" Authors.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package connection

import (
	"context"
	"errors"
	"sync"

	log "github.com/cihub/seelog"
	"github.com/mysteriumnetwork/node/communication"
	"github.com/mysteriumnetwork/node/consumer"
	"github.com/mysteriumnetwork/node/firewall"
	"github.com/mysteriumnetwork/node/identity"
	"github.com/mysteriumnetwork/node/market"
	"github.com/mysteriumnetwork/node/metadata"
	"github.com/mysteriumnetwork/node/session"
	"github.com/mysteriumnetwork/node/session/balance"
	"github.com/mysteriumnetwork/node/session/promise"
)

const managerLogPrefix = "[connection-manager] "

var (
	// ErrNoConnection error indicates that action applied to manager expects active connection (i.e. disconnect)
	ErrNoConnection = errors.New("no connection exists")
	// ErrAlreadyExists error indicates that action applied to manager expects no active connection (i.e. connect)
	ErrAlreadyExists = errors.New("connection already exists")
	// ErrConnectionCancelled indicates that connection in progress was cancelled by request of api user
	ErrConnectionCancelled = errors.New("connection was cancelled")
	// ErrConnectionFailed indicates that Connect method didn't reach "Connected" phase due to connection error
	ErrConnectionFailed = errors.New("connection has failed")
	// ErrUnsupportedServiceType indicates that target proposal contains unsupported service type
	ErrUnsupportedServiceType = errors.New("unsupported service type in proposal")
)

// Creator creates new connection by given options and uses state channel to report state changes
type Creator func(serviceType string, stateChannel StateChannel, statisticsChannel StatisticsChannel) (Connection, error)

// SessionInfo contains all the relevant info of the current session
type SessionInfo struct {
	SessionID  session.ID
	ConsumerID identity.Identity
	Proposal   market.ServiceProposal
}

// Publisher is responsible for publishing given events
type Publisher interface {
	Publish(topic string, args ...interface{})
}

// PaymentIssuer handles the payments for service
type PaymentIssuer interface {
	Start() error
	Stop()
}

// PaymentIssuerFactory creates a new payment issuer from the given params
type PaymentIssuerFactory func(initialState promise.State, messageChan chan balance.Message, dialog communication.Dialog, consumer, provider identity.Identity) (PaymentIssuer, error)

type connectionManager struct {
	//these are passed on creation
	newDialog            DialogCreator
	paymentIssuerFactory PaymentIssuerFactory
	newConnection        Creator
	eventPublisher       Publisher

	//these are populated by Connect at runtime
	ctx             context.Context
	mutex           sync.RWMutex
	status          Status
	sessionInfo     SessionInfo
	cleanConnection func()
}

// NewManager creates connection manager with given dependencies
func NewManager(
	dialogCreator DialogCreator,
	paymentIssuerFactory PaymentIssuerFactory,
	connectionCreator Creator,
	eventPublisher Publisher,
) *connectionManager {
	return &connectionManager{
		newDialog:            dialogCreator,
		paymentIssuerFactory: paymentIssuerFactory,
		newConnection:        connectionCreator,
		status:               statusNotConnected(),
		cleanConnection:      warnOnClean,
		eventPublisher:       eventPublisher,
	}
}

func (manager *connectionManager) Connect(consumerID identity.Identity, proposal market.ServiceProposal, params ConnectParams) (err error) {
	if manager.status.State != NotConnected {
		return ErrAlreadyExists
	}

	manager.mutex.Lock()
	manager.ctx, manager.cleanConnection = context.WithCancel(context.Background())
	manager.status = statusConnecting()
	manager.mutex.Unlock()
	defer func() {
		if err != nil {
			manager.mutex.Lock()
			manager.status = statusNotConnected()
			manager.mutex.Unlock()
		}
	}()

	err = manager.startConnection(consumerID, proposal, params)
	if err == context.Canceled {
		return ErrConnectionCancelled
	}
	return err
}

func (manager *connectionManager) startConnection(consumerID identity.Identity, proposal market.ServiceProposal, params ConnectParams) (err error) {
	manager.mutex.Lock()
	cancelCtx := manager.cleanConnection
	manager.mutex.Unlock()

	var cancel []func()
	defer func() {
		manager.cleanConnection = func() {
			cancelCtx()
			for i := range cancel { // Cancelling in a reverse order to keep correct workflow.
				cancel[len(cancel)-i-1]()
			}
		}
		if err != nil {
			log.Info(managerLogPrefix, "Cancelling connection initiation")
			_ = manager.Disconnect()
		}
	}()

	providerID := identity.FromAddress(proposal.ProviderID)
	dialog, err := manager.newDialog(consumerID, providerID, proposal.ProviderContacts[0])
	if err != nil {
		return err
	}
	cancel = append(cancel, func() { dialog.Close() })

	stateChannel := make(chan State, 10)
	statisticsChannel := make(chan consumer.SessionStatistics, 10)

	connection, err := manager.newConnection(proposal.ServiceType, stateChannel, statisticsChannel)
	if err != nil {
		return err
	}

	sessionCreateConfig, err := connection.GetConfig()
	if err != nil {
		return err
	}

	messageChan := make(chan balance.Message, 1)

	// TODO: load initial promise state
	payments, err := manager.paymentIssuerFactory(promise.State{}, messageChan, dialog, consumerID, providerID)
	if err != nil {
		return err
	}

	cancel = append(cancel, func() { payments.Stop() })

	go manager.payForService(payments)

	consumerInfo := session.ConsumerInfo{
		// TODO: once we're supporting payments from another identity make the changes accordingly
		IssuerID:          consumerID,
		MystClientVersion: metadata.VersionAsString(),
	}

	sessionID, sessionConfig, err := session.RequestSessionCreate(dialog, proposal.ID, sessionCreateConfig, consumerInfo)
	if err != nil {
		return err
	}

	cancel = append(cancel, func() { session.RequestSessionDestroy(dialog, sessionID) })

	// set the session info for future use
	manager.sessionInfo = SessionInfo{
		SessionID:  sessionID,
		ConsumerID: consumerID,
		Proposal:   proposal,
	}

	manager.eventPublisher.Publish(SessionEventTopic, SessionEvent{
		Status:      SessionCreatedStatus,
		SessionInfo: manager.sessionInfo,
	})

	cancel = append(cancel, func() {
		manager.eventPublisher.Publish(SessionEventTopic, SessionEvent{
			Status:      SessionEndedStatus,
			SessionInfo: manager.sessionInfo,
		})
	})

	connectOptions := ConnectOptions{
		SessionID:     sessionID,
		SessionConfig: sessionConfig,
		ConsumerID:    consumerID,
		ProviderID:    providerID,
		Proposal:      proposal,
	}

	if err = connection.Start(connectOptions); err != nil {
		return err
	}
	cancel = append(cancel, connection.Stop)

	//consume statistics right after start - openvpn3 will publish them even before connected state
	go manager.consumeStats(statisticsChannel)
	err = manager.waitForConnectedState(stateChannel, sessionID)
	if err != nil {
		return err
	}

	if !params.DisableKillSwitch {
		// TODO: Implement fw based kill switch for respective OS
		// we may need to wait for tun device setup to be finished
		firewall.NewKillSwitch().Enable()
	}

	go manager.consumeConnectionStates(stateChannel)
	go manager.connectionWaiter(connection)
	return nil
}

func (manager *connectionManager) Status() Status {
	manager.mutex.RLock()
	defer manager.mutex.RUnlock()

	return manager.status
}

func (manager *connectionManager) Disconnect() error {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	if manager.status.State == NotConnected {
		return ErrNoConnection
	}

	manager.status = statusDisconnecting()
	manager.cleanConnection()
	manager.status = statusNotConnected()

	return nil
}

func (manager *connectionManager) payForService(payments PaymentIssuer) {
	err := payments.Start()
	if err != nil {
		log.Error(managerLogPrefix, "payment error: ", err)
		err = manager.Disconnect()
		if err != nil {
			log.Error(managerLogPrefix, "could not disconnect gracefully:", err)
		}
	}
}

func warnOnClean() {
	log.Warn(managerLogPrefix, "Trying to close when there is nothing to close. Possible bug or race condition")
}

func (manager *connectionManager) connectionWaiter(connection Connection) {
	err := connection.Wait()
	if err != nil {
		log.Warn(managerLogPrefix, "Connection exited with error: ", err)
	} else {
		log.Info(managerLogPrefix, "Connection exited")
	}

	_ = manager.Disconnect()
}

func (manager *connectionManager) waitForConnectedState(stateChannel <-chan State, sessionID session.ID) error {
	for {
		select {
		case state, more := <-stateChannel:
			if !more {
				return ErrConnectionFailed
			}

			switch state {
			case Connected:
				manager.onStateChanged(state)
				return nil
			default:
				manager.onStateChanged(state)
			}
		case <-manager.ctx.Done():
			return manager.ctx.Err()
		}
	}
}

func (manager *connectionManager) consumeConnectionStates(stateChannel <-chan State) {
	for state := range stateChannel {
		manager.onStateChanged(state)
	}

	log.Debug(managerLogPrefix, "State updater stopCalled")
	_ = manager.Disconnect()
}

func (manager *connectionManager) consumeStats(statisticsChannel <-chan consumer.SessionStatistics) {
	for stats := range statisticsChannel {
		manager.eventPublisher.Publish(StatisticsEventTopic, stats)
	}
}

func (manager *connectionManager) onStateChanged(state State) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	manager.eventPublisher.Publish(StateEventTopic, StateEvent{
		State:       state,
		SessionInfo: manager.sessionInfo,
	})

	switch state {
	case Connected:
		manager.status = statusConnected(manager.sessionInfo.SessionID, manager.sessionInfo.Proposal)
	case Reconnecting:
		manager.status = statusReconnecting()
	}
}
