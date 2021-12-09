// Copyright 2021 Converter Systems LLC. All rights reserved.

package client_test

import (
	"context"
	"fmt"

	"github.com/awcullen/opcua"
	"github.com/awcullen/opcua/client"
)

// This example demonstrates reading the 'ServerStatus' variable.
func ExampleClient_Read() {

	ctx := context.Background()

	// open a connection to the testserver
	ch, err := client.Dial(
		ctx,
		"opc.tcp://localhost:46010",
		client.WithInsecureSkipVerify(), // skips verification of server certificate
	)
	if err != nil {
		fmt.Printf("Error opening client connection. %s\n", err.Error())
		return
	}

	// prepare read request
	req := &opcua.ReadRequest{
		NodesToRead: []opcua.ReadValueID{
			{
				NodeID:      opcua.VariableIDServerServerStatus,
				AttributeID: opcua.AttributeIDValue,
			},
		},
	}

	// send request to server. receive response or error
	res, err := ch.Read(ctx, req)
	if err != nil {
		fmt.Printf("Error reading ServerStatus. %s\n", err.Error())
		ch.Abort(ctx)
		return
	}

	// print results
	if serverStatus, ok := res.Results[0].Value.(opcua.ServerStatusDataType); ok {
		fmt.Printf("Server status:\n")
		fmt.Printf("  ProductName: %s\n", serverStatus.BuildInfo.ProductName)
		fmt.Printf("  ManufacturerName: %s\n", serverStatus.BuildInfo.ManufacturerName)
		fmt.Printf("  State: %s\n", serverStatus.State)
	} else {
		fmt.Println("Error decoding ServerStatus.")
	}

	// close connection
	err = ch.Close(ctx)
	if err != nil {
		ch.Abort(ctx)
		return
	}

	// Output:
	// Server status:
	//   ProductName: testserver
	//   ManufacturerName: awcullen
	//   State: Running
}