// Independent crypto/tls peer. All keys and traffic are synthetic and temporary.
package main

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func main() {
	if os.Args[1] == "cert" {
		var key crypto.Signer
		var err error
		switch os.Args[3] {
		case "p256":
			key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		case "p384":
			key, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		case "rsa":
			key, err = rsa.GenerateKey(rand.Reader, 2048)
		case "ed25519":
			_, key, err = ed25519.GenerateKey(rand.Reader)
		default:
			panic("unknown key")
		}
		must(err)
		template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "stulp.test"}, DNSNames: []string{"stulp.test", "localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
		cert, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
		must(err)
		der, err := x509.MarshalPKCS8PrivateKey(key)
		must(err)
		must(os.WriteFile(filepath.Join(os.Args[2], "cert.der"), cert, 0600))
		must(os.WriteFile(filepath.Join(os.Args[2], "key.der"), der, 0600))
		must(os.WriteFile(filepath.Join(os.Args[2], "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), 0600))
		must(os.WriteFile(filepath.Join(os.Args[2], "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600))
		switch key := key.(type) {
		case *ecdsa.PrivateKey:
			legacy, err := x509.MarshalECPrivateKey(key)
			must(err)
			must(os.WriteFile(filepath.Join(os.Args[2], "legacy.der"), legacy, 0600))
		case *rsa.PrivateKey:
			must(os.WriteFile(filepath.Join(os.Args[2], "legacy.der"), x509.MarshalPKCS1PrivateKey(key), 0600))
		default:
			os.Remove(filepath.Join(os.Args[2], "legacy.der"))
		}

		return
	}
	cert, err := os.ReadFile(filepath.Join(os.Args[2], "cert.der"))
	must(err)
	parsed, err := x509.ParseCertificate(cert)
	must(err)
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	config := &tls.Config{RootCAs: roots, ServerName: "stulp.test", MinVersion: tls.VersionTLS13, NextProtos: []string{"h2", "http/1.1"}}
	if os.Args[1] == "echo-p256" {
		config.CurvePreferences = []tls.CurveID{tls.CurveP256}
	}
	if os.Args[1] == "wrong-alpn" {
		config.NextProtos = []string{"h3"}
	}
	if os.Args[1] == "wrong-name" {
		config.ServerName = "wrong.test"
	}
	if os.Args[1] == "tls12" {
		config.MinVersion = tls.VersionTLS12
		config.MaxVersion = tls.VersionTLS12
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", os.Args[3], config)
	if !strings.HasPrefix(os.Args[1], "echo") {
		if err == nil {
			conn.Close()
			panic("invalid connection accepted")
		}
		fmt.Println("rejected")
		return
	}
	must(err)
	defer conn.Close()
	must(conn.SetDeadline(time.Now().Add(5 * time.Second)))
	if conn.ConnectionState().NegotiatedProtocol != "http/1.1" {
		panic("ALPN failed")
	}
	data := bytes.Repeat([]byte("independent TLS record test"), 1700)
	_, err = conn.Write(data)
	must(err)
	result := make([]byte, len(data))
	_, err = io.ReadFull(conn, result)
	must(err)
	if !bytes.Equal(data, result) {
		panic("echo mismatch")
	}
	// A graceful authenticated TLS EOF must be delivered after the complete response.
	extra := make([]byte, 1)
	n, err := conn.Read(extra)
	if n != 0 || err != io.EOF {
		panic(fmt.Sprintf("close_notify: %d %v", n, err))
	}
	fmt.Println("ok")
}
