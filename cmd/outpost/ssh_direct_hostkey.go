package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/qiangli/yoke/pkg/sshclient"
	"golang.org/x/crypto/ssh"
)

// Direct TCP dials pin each host:port independently and refuse changed keys
// unless the operator explicitly opts out. The tunnel's host aliases and
// policy remain owned by sshclient. Unscoped legacy pins cannot identify a
// port, so a direct endpoint gets its own TOFU pin on first use.
func sshDirectHostKeyCallback(path, alias string) (ssh.HostKeyCallback, error) {
	if strings.TrimSpace(os.Getenv(sshclient.StrictHostKeyEnv)) == "0" {
		return sshclient.KnownHostsCallbackTOFU(path, alias)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	var mu sync.Mutex
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		mu.Lock()
		defer mu.Unlock()
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		found := false
		for len(data) > 0 {
			marker, hosts, pinned, _, rest, err := ssh.ParseKnownHosts(data)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return fmt.Errorf("read known_hosts %s: %w", path, err)
			}
			data = rest
			for _, host := range hosts {
				if host != alias {
					continue
				}
				if marker != "" {
					return fmt.Errorf("unsupported known_hosts marker %q for %q in %s", marker, alias, path)
				}
				if !bytes.Equal(pinned.Marshal(), key.Marshal()) {
					return fmt.Errorf("REMOTE HOST IDENTIFICATION HAS CHANGED for %q: presented key %s does not match pinned entry; verify the server key before removing the entry from %s", alias, ssh.FingerprintSHA256(key), path)
				}
				found = true
			}
		}
		if found {
			return nil
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = fmt.Fprintf(f, "%s %s", alias, ssh.MarshalAuthorizedKey(key))
		return err
	}, nil
}
