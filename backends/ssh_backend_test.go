package backends

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func generateSSHPrivateKey() (ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}

	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}

	signer, err := ssh.ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}))
	if err != nil {
		return nil, err
	}

	return signer, nil
}

// startSftpServer starts a very hacky sftp server based on
// https://github.com/pkg/sftp/blob/v1.13.5/examples/go-sftp-server/main.go
// It accepts user with password, or with one of the authorized keys.
func startSftpServer(t testing.TB, user string, password string, authorized ...ssh.PublicKey) net.Listener {
	t.Helper()

	config := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == user && string(pass) == password {
				return nil, nil
			}
			return nil, fmt.Errorf("password rejected for %q", c.User())
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			for _, k := range authorized {
				if c.User() == user && bytes.Equal(k.Marshal(), key.Marshal()) {
					return nil, nil
				}
			}
			return nil, fmt.Errorf("key rejected for %q", c.User())
		},
	}
	signer, err := generateSSHPrivateKey()
	if err != nil {
		t.Fatal("failed to generate key", err)
	}
	config.AddHostKey(signer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("failed to listen for connection", err)
	}

	fmt.Println("Listening on", listener.Addr())

	go func() {
		for {
			acceptedConn, err := listener.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					t.Fatal("failed to accept incoming connection", err)
				}
				return
			}

			go func(conn net.Conn) {
				// Before use, a handshake must be performed on the incoming net.Conn.
				_, chans, reqs, err := ssh.NewServerConn(conn, config)
				if err != nil {
					t.Fatal("failed to handshake", err)
				}
				fmt.Println("SSH server established")

				// The incoming Request channel must be serviced.
				go ssh.DiscardRequests(reqs)

				// Service the incoming Channel channel.
				for newChannel := range chans {
					// Channels have a type, depending on the application level
					// protocol intended. In the case of an SFTP session, this is "subsystem"
					// with a payload string of "<length=4>sftp"
					fmt.Println("Incoming channel:", newChannel.ChannelType())
					if newChannel.ChannelType() != "session" {
						_ = newChannel.Reject(ssh.UnknownChannelType, "unknown channel type")
						fmt.Println("Unknown channel type:", newChannel.ChannelType())
						continue
					}
					channel, requests, err := newChannel.Accept()
					if err != nil {
						t.Fatal("could not accept channel.", err)
					}
					fmt.Println("Channel accepted")

					// Sessions have out-of-band requests such as "shell",
					// "pty-req" and "env".  Here we handle only the
					// "subsystem" request.
					go func(in <-chan *ssh.Request) {
						for req := range in {
							fmt.Println("Request:", req.Type)
							ok := false
							if req.Type == "subsystem" {
								fmt.Printf("Subsystem: %s\n", req.Payload[4:])
								if string(req.Payload[4:]) == "sftp" {
									ok = true
								}
							}
							fmt.Println(" - accepted:", ok)
							_ = req.Reply(ok, nil)
						}
					}(requests)

					server, err := sftp.NewServer(channel)
					if err != nil {
						t.Fatal(err)
					}
					// pkg/sftp >= 1.13.6 returns nil, not io.EOF, when the client ends the session.
					serveErr := server.Serve()
					_ = server.Close()
					_ = channel.Close() // what sshd does when the subsystem exits
					if serveErr != nil && serveErr != io.EOF {
						t.Fatal("sftp server completed with error:", serveErr)
					}
				}
			}(acceptedConn)
		}
	}()

	return listener
}

func TestSSHBackend(t *testing.T) {
	t.Parallel()

	sftpListener := startSftpServer(t, "test", "password")
	defer func() {
		_ = sftpListener.Close()
	}()

	tempPath := t.TempDir()

	backendUriEnd := "test:password@" + sftpListener.Addr().String() + tempPath

	err := os.Setenv("SSH_KNOWN_HOSTS", "ignore")
	if err != nil {
		t.Fatalf("Failed to disable host key checking: %v", err)
	}

	b, err := GetBackendForURI(SSHBackendPrefix + "://" + backendUriEnd)
	if err != nil {
		t.Fatalf("Error while trying to get backend: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	BackendTest(ctx, SSHBackendPrefix, backendUriEnd, true, b)(t)
}

// TestSSHBackendAgentThenKeyFile: an ssh-agent whose key the server refuses must not stop
// SSH_KEY_FILE. The ssh client tries each method name once; with the agent and the key file as
// two publickey methods, the key file was never offered.
func TestSSHBackendAgentThenKeyFile(t *testing.T) {
	_, agentKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	filePub, fileKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authorized, err := ssh.NewPublicKey(filePub)
	if err != nil {
		t.Fatal(err)
	}
	listener := startSftpServer(t, "test", "", authorized)
	defer func() {
		_ = listener.Close()
	}()

	// Not t.TempDir(): a unix socket path must stay short.
	dir, err := os.MkdirTemp("", "agent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	keyring := agent.NewKeyring()
	if err = keyring.Add(agent.AddedKey{PrivateKey: agentKey}); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "sock")
	agentListener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = agentListener.Close()
	}()
	go func() {
		for {
			conn, aerr := agentListener.Accept()
			if aerr != nil {
				return
			}
			go func() { _ = agent.ServeAgent(keyring, conn) }()
		}
	}()

	block, err := ssh.MarshalPrivateKey(fileKey, "")
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "id_ed25519")
	if err = os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_AUTH_SOCK", sock)
	t.Setenv("SSH_KEY_FILE", keyFile)
	t.Setenv("SSH_KNOWN_HOSTS", "ignore")
	t.Setenv("SSH_PASSWORD", "")
	t.Setenv("SSH_USERNAME", "")

	uri := SSHBackendPrefix + "://test@" + listener.Addr().String() + t.TempDir()
	b, err := GetBackendForURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Init(context.Background(), &BackendConfig{TargetURI: uri}); err != nil {
		t.Fatalf("Init with an agent key the server refuses and an authorized SSH_KEY_FILE: %v", err)
	}
	_ = b.Close()
}
