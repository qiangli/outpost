package runtimecmd

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"

	"github.com/spf13/cobra"
)

func certgenCmd() *cobra.Command {
	var cert, key, ca, caKey string
	cmd := &cobra.Command{Use: "certgen loopback|metrics", Short: "Mint runtime TLS credentials", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error { return generateCertificate(args[0], cert, key, ca, caKey) }}
	cmd.Flags().StringVar(&cert, "cert", "", "Output certificate PEM")
	cmd.Flags().StringVar(&key, "key", "", "Output private key PEM")
	cmd.Flags().StringVar(&ca, "ca", "", "Metrics client CA certificate PEM")
	cmd.Flags().StringVar(&caKey, "ca-key", "", "Metrics client CA private key PEM")
	_ = cmd.MarkFlagRequired("cert")
	_ = cmd.MarkFlagRequired("key")
	return cmd
}

func generateCertificate(kind, certPath, keyPath, caPath, caKeyPath string) error {
	if certPath == "" || keyPath == "" || certPath == keyPath {
		return fmt.Errorf("distinct cert and key paths are required")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	serial.Add(serial, big.NewInt(1))
	now := time.Now()
	cert := &x509.Certificate{SerialNumber: serial, NotBefore: now.Add(-time.Minute), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment}
	var parent *x509.Certificate
	var signingKey any
	switch kind {
	case "loopback":
		if caPath != "" || caKeyPath != "" {
			return fmt.Errorf("loopback certificate is self-signed")
		}
		cert.Subject.CommonName = "overlay-control.local"
		cert.NotAfter = now.Add(3650 * 24 * time.Hour)
		cert.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		cert.DNSNames = []string{"localhost"}
		cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		parent = cert
	case "metrics":
		pair, err := tls.LoadX509KeyPair(caPath, caKeyPath)
		if err != nil {
			return fmt.Errorf("load client CA: %w", err)
		}
		parent, err = x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return err
		}
		if !parent.IsCA || parent.KeyUsage&x509.KeyUsageCertSign == 0 {
			return fmt.Errorf("client CA cannot sign certificates")
		}
		signingKey = pair.PrivateKey
		cert.Subject = pkix.Name{CommonName: "system:serviceaccount:kube-system:metrics-server", Organization: []string{"system:serviceaccounts", "system:serviceaccounts:kube-system", "system:authenticated"}}
		cert.NotAfter = now.Add(48 * time.Hour)
		if cert.NotAfter.After(parent.NotAfter) {
			cert.NotAfter = parent.NotAfter
		}
		if !cert.NotAfter.After(now) || parent.NotBefore.After(now) {
			return fmt.Errorf("client CA is not currently valid")
		}
		cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	default:
		return fmt.Errorf("unknown certificate kind %q", kind)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	if signingKey == nil {
		signingKey = key
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, parent, &key.PublicKey, signingKey)
	if err != nil {
		return err
	}
	if err := writePrivateFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644)
}

func writePrivateFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Close()
}
