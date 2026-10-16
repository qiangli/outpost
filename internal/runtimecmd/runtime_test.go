package runtimecmd

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func execute(ctx context.Context, args ...string) error {
	root := &cobra.Command{SilenceUsage: true, SilenceErrors: true}
	Register(root)
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}

func TestCertgen(t *testing.T) {
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(72 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPath, caKeyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	keyDER, _ := x509.MarshalPKCS8PrivateKey(caKey)
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caKeyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"loopback", "metrics"} {
		t.Run(kind, func(t *testing.T) {
			certPath, keyPath := filepath.Join(dir, kind+".crt"), filepath.Join(dir, kind+".key")
			args := []string{"certgen", kind, "--cert", certPath, "--key", keyPath}
			if kind == "metrics" {
				args = append(args, "--ca", caPath, "--ca-key", caKeyPath)
			}
			if err := execute(context.Background(), args...); err != nil {
				t.Fatal(err)
			}
			pair, err := tls.LoadX509KeyPair(certPath, keyPath)
			if err != nil {
				t.Fatal(err)
			}
			cert, err := x509.ParseCertificate(pair.Certificate[0])
			if err != nil {
				t.Fatal(err)
			}
			info, _ := os.Stat(keyPath)
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
				t.Fatalf("key mode %v", info.Mode())
			}
			roots := x509.NewCertPool()
			usage := x509.ExtKeyUsageServerAuth
			opts := x509.VerifyOptions{Roots: roots}
			if kind == "loopback" {
				roots.AddCert(cert)
				opts.DNSName = "127.0.0.1"
				if err := cert.VerifyHostname("localhost"); err != nil {
					t.Fatal(err)
				}
				if cert.Subject.CommonName != "overlay-control.local" {
					t.Fatal(cert.Subject)
				}
			} else {
				parent, _ := x509.ParseCertificate(der)
				roots.AddCert(parent)
				usage = x509.ExtKeyUsageClientAuth
				slices.Sort(cert.Subject.Organization)
				if cert.Subject.CommonName != "system:serviceaccount:kube-system:metrics-server" || !slices.Equal(cert.Subject.Organization, []string{"system:authenticated", "system:serviceaccounts", "system:serviceaccounts:kube-system"}) {
					t.Fatal(cert.Subject)
				}
				if cert.IsCA || time.Until(cert.NotAfter) > 48*time.Hour {
					t.Fatal("incorrect client constraints")
				}
			}
			opts.KeyUsages = []x509.ExtKeyUsage{usage}
			if _, err := cert.Verify(opts); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := execute(context.Background(), "certgen", "metrics", "--cert", filepath.Join(dir, "bad.crt"), "--key", filepath.Join(dir, "bad.key")); err == nil {
		t.Fatal("accepted missing CA")
	}
}

func echoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return ln
}

func TestTLSProxy(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			backend := echoServer(t)
			var cert, key string
			if encrypted {
				dir := t.TempDir()
				cert, key = filepath.Join(dir, "cert"), filepath.Join(dir, "key")
				if err := execute(context.Background(), "certgen", "loopback", "--cert", cert, "--key", key); err != nil {
					t.Fatal(err)
				}
			}
			ln, err := proxyListener("127.0.0.1:0", cert, key)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- serveProxy(ctx, ln, backend.Addr().String()) }()
			var c net.Conn
			if encrypted {
				data, _ := os.ReadFile(cert)
				pool := x509.NewCertPool()
				pool.AppendCertsFromPEM(data)
				c, err = tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12})
			} else {
				c, err = net.Dial("tcp", ln.Addr().String())
			}
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err = c.Write([]byte("hello")); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, 5)
			if _, err = io.ReadFull(c, got); err != nil {
				t.Fatal(err)
			}
			if string(got) != "hello" {
				t.Fatal(string(got))
			}
			// Termination must also close active connections, not just the listener.
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("proxy shutdown hung")
			}
		})
	}
	if _, err := proxyListener("127.0.0.1:0", "cert", ""); err == nil {
		t.Fatal("accepted incomplete TLS credentials")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestFRPCommands(t *testing.T) {
	dir := t.TempDir()
	backend := echoServer(t)
	port, remote := freePort(t), freePort(t)
	for remote == port {
		remote = freePort(t)
	}
	serverPath, clientPath := filepath.Join(dir, "frps.toml"), filepath.Join(dir, "frpc.toml")
	serverConfig := fmt.Sprintf("bindAddr = '127.0.0.1'\nbindPort = %d\nproxyBindAddr = '127.0.0.1'\nauth.token = 'test-token'\nlog.level = 'error'\n", port)
	clientConfig := fmt.Sprintf("serverAddr = '127.0.0.1'\nserverPort = %d\nauth.token = 'test-token'\nloginFailExit = false\nlog.level = 'error'\n[[proxies]]\nname = 'echo'\ntype = 'tcp'\nlocalIP = '127.0.0.1'\nlocalPort = %d\nremotePort = %d\n", port, backend.Addr().(*net.TCPAddr).Port, remote)
	for path, data := range map[string]string{serverPath: serverConfig, clientPath: clientConfig} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverDone, clientDone := make(chan error, 1), make(chan error, 1)
	go func() { serverDone <- execute(ctx, "frps", "-c", serverPath) }()
	// Wait for the server before starting the client (FRP logger is global).
	waitDial(t, fmt.Sprintf("127.0.0.1:%d", port), serverDone)
	go func() { clientDone <- execute(ctx, "frpc", "-c", clientPath) }()
	addr := fmt.Sprintf("127.0.0.1:%d", remote)
	waitDial(t, addr, clientDone)
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte("tunnel"))
	got := make([]byte, 6)
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "tunnel" {
		t.Fatal(string(got))
	}
	cancel()
	for _, done := range []chan error{clientDone, serverDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("FRP shutdown hung")
		}
	}
	for _, name := range []string{"frpc", "frps"} {
		if err := execute(context.Background(), name, "-c", filepath.Join(dir, "missing")); err == nil {
			t.Fatal("accepted missing config")
		}
	}
}

func waitDial(t *testing.T, addr string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("service stopped: %v", err)
		default:
		}
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("listener never opened: %s", addr)
}

func TestNetaddr(t *testing.T) {
	for _, args := range [][]string{{"add", "bad", "dev", "lo"}, {"add", "127.0.0.1/32", "wrong", "lo"}, {"add"}, {"exists", "outpost-no-such-interface"}} {
		cmd := netaddrCmd()
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			cmd := netaddrCmd()
			cmd.SetArgs([]string{"exists", iface.Name})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			cmd = netaddrCmd()
			cmd.SetOut(&out)
			cmd.SetArgs([]string{"ipv4", iface.Name})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			ip := net.ParseIP(string(bytes.TrimSpace(out.Bytes())))
			if ip == nil || !ip.IsLoopback() {
				t.Fatal(out.String())
			}
			return
		}
	}
	t.Fatal("no loopback interface")
}

func TestNetaddrAdd(t *testing.T) {
	called := false
	cmd := netaddrCommand(func(cidr, name string) error {
		called = true
		if cidr != "192.0.2.10/32" || name != "lo" {
			t.Fatalf("unexpected address %s on %s", cidr, name)
		}
		return nil
	})
	cmd.SetArgs([]string{"add", "192.0.2.10/32", "dev", "lo"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("did not call address backend")
	}
}

func TestRuntimeCommandErrors(t *testing.T) {
	for _, args := range [][]string{{"frpc"}, {"frps"}, {"tlsproxy"}, {"tlsproxy", "--listen", "127.0.0.1:0", "--forward", "127.0.0.1:1", "--cert", "missing"}, {"certgen", "unknown", "--cert", "unused", "--key", "unused"}} {
		if err := execute(context.Background(), args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
