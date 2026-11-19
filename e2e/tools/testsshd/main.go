// testsshd 启动只接受指定私钥的本机 SSH 测试服务。它把私钥写到命令行指
// 定路径，向标准输出打印实际监听地址，供 Playwright 测试进程读取。
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"

	"golang.org/x/crypto/ssh"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "用法：testsshd <私钥输出路径>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(keyPath string) error {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		return err
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return err
	}

	configuration := &ssh.ServerConfig{}
	configuration.AddHostKey(signer)
	allowedKey := signer.PublicKey().Marshal()
	configuration.PublicKeyCallback = func(_ ssh.ConnMetadata, presented ssh.PublicKey) (*ssh.Permissions, error) {
		if string(presented.Marshal()) == string(allowedKey) {
			return nil, nil
		}
		return nil, io.EOF
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	fmt.Println(listener.Addr().String())
	_ = os.Stdout.Sync()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	go func() {
		<-stop
		_ = listener.Close()
	}()
	for {
		connection, err := listener.Accept()
		if err != nil {
			return nil
		}
		go serve(connection, configuration)
	}
}

func serve(connection net.Conn, configuration *ssh.ServerConfig) {
	defer connection.Close()
	server, channels, requests, err := ssh.NewServerConn(connection, configuration)
	if err != nil {
		return
	}
	defer server.Close()
	go ssh.DiscardRequests(requests)
	for channel := range channels {
		if channel.ChannelType() != "session" {
			_ = channel.Reject(ssh.UnknownChannelType, "只支持会话通道")
			continue
		}
		accepted, requests, err := channel.Accept()
		if err != nil {
			continue
		}
		go serveSession(accepted, requests)
	}
}

func serveSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	done := make(chan struct{})
	go func() {
		for request := range requests {
			if request.WantReply {
				_ = request.Reply(request.Type == "exec" || request.Type == "shell", nil)
			}
			if request.Type == "exec" {
				_, _ = channel.Write(nil)
				_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				close(done)
				break
			}
		}
	}()
	<-done
}
