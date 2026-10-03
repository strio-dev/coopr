package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

func main() {
	var repository, clientPublicKeyPath, hostPrivateKeyPath string
	flag.StringVar(&repository, "repository", "", "bare Git repository served by git-upload-pack")
	flag.StringVar(&clientPublicKeyPath, "client-public-key", "", "authorized client public key")
	flag.StringVar(&hostPrivateKeyPath, "host-private-key", "", "SSH host private key")
	flag.Parse()
	if repository == "" || clientPublicKeyPath == "" || hostPrivateKeyPath == "" {
		fatal(errors.New("repository, client-public-key, and host-private-key are required"))
	}

	clientPublicKey, err := readPublicKey(clientPublicKeyPath)
	if err != nil {
		fatal(err)
	}
	hostPrivateKey, err := os.ReadFile(hostPrivateKeyPath)
	if err != nil {
		fatal(fmt.Errorf("read host private key: %w", err))
	}
	hostSigner, err := ssh.ParsePrivateKey(hostPrivateKey)
	if err != nil {
		fatal(fmt.Errorf("parse host private key: %w", err))
	}

	server := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) != string(clientPublicKey.Marshal()) {
				return nil, errors.New("unauthorized public key")
			}
			return nil, nil
		},
	}
	server.AddHostKey(hostSigner)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fatal(fmt.Errorf("listen: %w", err))
	}
	defer listener.Close()
	if _, err := fmt.Fprintln(os.Stdout, listener.Addr().String()); err != nil {
		fatal(fmt.Errorf("report listener: %w", err))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var connections sync.WaitGroup
	defer connections.Wait()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			fatal(fmt.Errorf("accept: %w", err))
		}
		connections.Add(1)
		go func() {
			defer connections.Done()
			serveConnection(ctx, connection, server, repository)
		}()
	}
}

func readPublicKey(path string) (ssh.PublicKey, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read client public key: %w", err)
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey(contents)
	if err != nil {
		return nil, fmt.Errorf("parse client public key: %w", err)
	}
	return key, nil
}

func serveConnection(ctx context.Context, connection net.Conn, config *ssh.ServerConfig, repository string) {
	serverConnection, channels, requests, err := ssh.NewServerConn(connection, config)
	if err != nil {
		_ = connection.Close()
		return
	}
	defer serverConnection.Close()
	go ssh.DiscardRequests(requests)
	for channelRequest := range channels {
		if channelRequest.ChannelType() != "session" {
			_ = channelRequest.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		channel, requests, err := channelRequest.Accept()
		if err != nil {
			continue
		}
		go serveSession(ctx, channel, requests, repository)
	}
}

func serveSession(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request, repository string) {
	defer channel.Close()
	for request := range requests {
		switch request.Type {
		case "env":
			_ = request.Reply(true, nil)
		case "exec":
			var payload struct{ Command string }
			if err := ssh.Unmarshal(request.Payload, &payload); err != nil || !strings.HasPrefix(payload.Command, "git-upload-pack ") {
				_ = request.Reply(false, nil)
				return
			}
			if err := request.Reply(true, nil); err != nil {
				return
			}
			command := exec.CommandContext(ctx, "git-upload-pack", "--strict", repository)
			command.Stdin = channel
			command.Stdout = channel
			command.Stderr = channel.Stderr()
			err := command.Run()
			status := uint32(0)
			if err != nil {
				status = 1
				var exitError *exec.ExitError
				if errors.As(err, &exitError) {
					status = uint32(exitError.ExitCode())
				}
			}
			exitStatus := make([]byte, 4)
			binary.BigEndian.PutUint32(exitStatus, status)
			_, _ = channel.SendRequest("exit-status", false, exitStatus)
			return
		default:
			_ = request.Reply(false, nil)
		}
	}
}

func fatal(err error) {
	_, _ = fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
