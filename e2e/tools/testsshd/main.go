// testsshd 启动只接受指定私钥的本机 SSH 测试服务。它把私钥写到命令行指
// 定路径，向标准输出打印实际监听地址，供 Playwright 测试进程读取；标准
// 输入管道关闭或收到 SIGINT/SIGTERM 时关监听退出，不会脱离管理常驻。
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
	"strconv"
	"syscall"
	"time"

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
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		_ = listener.Close()
	}()
	watchStandardInputClose(listener)
	handshakeDelay := time.Duration(0)
	if delayText := os.Getenv("SSHMGR_E2E_TEST_SSHD_DELAY_MS"); delayText != "" {
		delayMilliseconds, err := strconv.Atoi(delayText)
		if err != nil || delayMilliseconds < 0 {
			return fmt.Errorf("SSHMGR_E2E_TEST_SSHD_DELAY_MS 必须是非负整数毫秒数")
		}
		handshakeDelay = time.Duration(delayMilliseconds) * time.Millisecond
	}
	for {
		connection, err := listener.Accept()
		if err != nil {
			return nil
		}
		go serve(connection, configuration, handshakeDelay)
	}
}

// watchStandardInputClose 在标准输入是管道时监视它关闭：测试助手直接拉起
// 本工具并把标准输入接在自己的管道上，测试进程退出（无论正常收尾还是崩
// 溃）后管道关闭，这里立即关监听让本工具自行退出。没有这条路径，测试进
// 程意外终止时本工具会脱离管理常驻并继续监听端口（#318）。
func watchStandardInputClose(listener net.Listener) {
	information, err := os.Stdin.Stat()
	if err != nil || information.Mode()&os.ModeNamedPipe == 0 {
		return
	}
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		_ = listener.Close()
	}()
}

func serve(connection net.Conn, configuration *ssh.ServerConfig, handshakeDelay time.Duration) {
	defer connection.Close()
	if handshakeDelay > 0 {
		time.Sleep(handshakeDelay)
	}
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
