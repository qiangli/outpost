package runtimecmd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The hosted plane owns a single etcd member. Its data volume survives network
// namespace replacement, but etcd's persisted peer URL does not follow the IP.
func etcdPeerCmd() *cobra.Command {
	return &cobra.Command{Use: "etcd-peer ADDRESS", Short: "Reconcile the hosted plane's single etcd peer address", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if net.ParseIP(args[0]) == nil {
			return fmt.Errorf("invalid peer IP address")
		}
		const dir = "/var/lib/rancher/k3s/server"
		name, err := os.ReadFile(filepath.Join(dir, "db/etcd/name"))
		if err != nil {
			return err
		}
		cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "tls/etcd/client.crt"), filepath.Join(dir, "tls/etcd/client.key"))
		if err != nil {
			return err
		}
		ca, err := os.ReadFile(filepath.Join(dir, "tls/etcd/server-ca.crt"))
		if err != nil {
			return err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(ca) {
			return fmt.Errorf("invalid etcd CA")
		}
		transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: []tls.Certificate{cert}}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
		return reconcileEtcdPeer(cmd.Context(), client, "https://127.0.0.1:2382", strings.TrimSpace(string(name)), "https://"+net.JoinHostPort(args[0], "2380"))
	}}
}

type etcdMember struct {
	ID        string   `json:"ID"`
	Name      string   `json:"name"`
	PeerURLs  []string `json:"peerURLs"`
	IsLearner bool     `json:"isLearner"`
}

func reconcileEtcdPeer(ctx context.Context, client *http.Client, endpoint, name, peer string) error {
	post := func(path string, body any, result any) error {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", endpoint+path, bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("etcd %s: HTTP %d", path, resp.StatusCode)
		}
		return json.NewDecoder(resp.Body).Decode(result)
	}
	var list struct {
		Members []etcdMember `json:"members"`
	}
	if err := post("/v3/cluster/member/list", struct{}{}, &list); err != nil {
		return err
	}
	if len(list.Members) != 1 || name == "" || list.Members[0].Name != name || list.Members[0].IsLearner || list.Members[0].ID == "" {
		return fmt.Errorf("refusing peer update: expected one local voting etcd member")
	}
	member := list.Members[0]
	if len(member.PeerURLs) == 1 && member.PeerURLs[0] == peer {
		return nil
	}
	var updated struct {
		Members []etcdMember `json:"members"`
	}
	if err := post("/v3/cluster/member/update", struct {
		ID       string   `json:"ID"`
		PeerURLs []string `json:"peerURLs"`
	}{member.ID, []string{peer}}, &updated); err != nil {
		return err
	}
	for _, m := range updated.Members {
		if m.ID == member.ID && len(m.PeerURLs) == 1 && m.PeerURLs[0] == peer {
			return nil
		}
	}
	return fmt.Errorf("etcd did not confirm the updated peer address")
}
