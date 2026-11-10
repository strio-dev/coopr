// This independent consumer uses only Coopr's public, native-free client.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/strio-dev/coopr/api"
	"github.com/strio-dev/coopr/client"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("client-consumer", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socket := flags.String("socket", "", "absolute socket path of a caller-owned Coopr service")
	requestPath := flags.String("request", "-", "API request JSON file; - reads stdin")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	connection, err := client.New(*socket, client.Options{})
	if err != nil {
		return err
	}
	defer connection.CloseIdleConnections()
	input := stdin
	if *requestPath != "-" {
		file, err := os.Open(*requestPath)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		input = file
	}
	request, err := readRequest(input)
	if err != nil {
		return err
	}
	result, executeErr := connection.Execute(ctx, request, client.StreamOptions{Stdout: stderr, Stderr: stderr})
	// Publication can return useful per-destination outcomes alongside an error.
	// Keep those results separate from logs, and preserve the failure exit status.
	encodeErr := json.NewEncoder(stdout).Encode(result)
	return errors.Join(executeErr, encodeErr)
}

func readRequest(input io.Reader) (api.Request, error) {
	data, err := io.ReadAll(io.LimitReader(input, api.MaxRequestBytes+1))
	if err != nil {
		return api.Request{}, err
	}
	if len(data) > api.MaxRequestBytes {
		return api.Request{}, errors.New("request exceeds API size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var request *api.Request
	if err := decoder.Decode(&request); err != nil {
		return api.Request{}, err
	}
	if request == nil {
		return api.Request{}, errors.New("request must be a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return api.Request{}, errors.New("request must contain exactly one JSON object")
	}
	return *request, nil
}
