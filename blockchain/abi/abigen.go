package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
)

var pkgName = flag.String("pkg", "", "Same as abigen tool from ethereum project")
var output = flag.String("out", "", "Filename where to write generated code. Unspecified - stdout")
var input = flag.String("in", "", "Filename of truffle compiled smart contract (json format)")

func main() {
	flag.Parse()
	if *pkgName == "" {
		fmt.Println("package name missing(--pkg)")
		os.Exit(-1)
	}

	if *input == "" {
		fmt.Println("input filename is missing")
		os.Exit(-1)
	}

	smartContract, err := parseTruffleArtifact(*input)
	if err != nil {
		fmt.Println("Error parsing truffle output: ", err.Error())
		os.Exit(-1)
	}

	genCode, err := bind.Bind([]string{smartContract.ContractName}, []string{smartContract.AbiString()}, []string{smartContract.Bytecode}, *pkgName, bind.LangGo)
	if err != nil {
		fmt.Println("Error: ", err.Error())
		os.Exit(-1)
	}
	writer := os.Stdout
	if *output != "" {
		writer, err = os.Create(*output)
		if err != nil {
			fmt.Println("Error:", err.Error())
			os.Exit(1)
		}
		defer writer.Close()
	}
	_, err = io.WriteString(writer, genCode)
	if err != nil {
		fmt.Println("Error:", err.Error())
	}
}

func parseTruffleArtifact(input string) (TruffleOutput, error) {
	reader, err := os.Open(input)
	if err != nil {
		return TruffleOutput{}, err
	}
	var output TruffleOutput
	err = json.NewDecoder(reader).Decode(&output)
	if err != nil {
		return TruffleOutput{}, err
	}
	return output, nil
}

type TruffleOutput struct {
	Bytecode     string          `json:"bytecode"`
	AbiBytes     json.RawMessage `json:"abi"`
	ContractName string          `json:"contractName"`
}

func (to TruffleOutput) AbiString() string {
	return string(to.AbiBytes)
}
