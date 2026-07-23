// Copyright 2021-2026 The Connect Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package internal

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"
)

const (
	ClientCertName = "Conformance Client"
	ServerCertName = "Conformance Server"
)

// NewClientTLSConfig returns a TLS configuration for an RPC client that uses
// the given PEM-encoded certs/keys. The caCert parameter must not be empty, and
// is the server certificate to trust (or a CA cert for the issuer of the server
// cert). The clientCert and clientKey parameters are optional. If one is provided
// then both must be present. They enable the use of a client certificate during
// the TLS handshake, for mutually-authenticated TLS.
func NewClientTLSConfig(caCert, clientCert, clientKey []byte) (*tls.Config, error) {
	if len(caCert) == 0 {
		return nil, errors.New("caCert is empty")
	}
	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, errors.New("failed to parse CA cert from given data")
	}

	hasClientCert := len(clientCert) != 0
	hasClientKey := len(clientKey) != 0
	var certs []tls.Certificate
	switch {
	case hasClientCert && hasClientKey:
		certPair, err := ParseServerCert(clientCert, clientKey)
		if err != nil {
			return nil, err
		}
		certs = []tls.Certificate{certPair}
	case hasClientCert:
		return nil, errors.New("clientCert is not empty but clientKey is")
	case hasClientKey:
		return nil, errors.New("clientKey is not empty but clientCert is")
	}

	return &tls.Config{
		RootCAs:      caCertPool,
		Certificates: certs,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// NewServerTLSConfig returns a TLS configuration for an RPC server that uses
// the given PEM-encoded cert/key. If the cert and key parameters are required.
// The clientCACert parameter is required unless clientCerts is tls.NoClientCert.
func NewServerTLSConfig(cert tls.Certificate, clientCertMode tls.ClientAuthType, clientCACert []byte) (*tls.Config, error) {
	if clientCertMode != tls.NoClientCert && len(clientCACert) == 0 {
		return nil, errors.New("clientCertMode indicates client certs supported but CACert is empty")
	}
	var caCertPool *x509.CertPool
	if len(clientCACert) > 0 {
		caCertPool = x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(clientCACert) {
			return nil, errors.New("failed to parse client CA cert from given data")
		}
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    caCertPool,
		ClientAuth:   clientCertMode,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ParseServerCert parses the given PEM-encoded cert and key.
func ParseServerCert(cert, key []byte) (tls.Certificate, error) {
	if len(cert) == 0 {
		return tls.Certificate{}, errors.New("cert is empty")
	}
	if len(key) == 0 {
		return tls.Certificate{}, errors.New("key is empty")
	}
	certPair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPair.Leaf, err = x509.ParseCertificate(certPair.Certificate[0])
	if err != nil {
		return tls.Certificate{}, err
	}
	return certPair, nil
}

// NewServerCert generates a new self-signed certificate. The first
// return value is usable with a *tls.Config. The next two values are
// the PEM-encoded certificate (which must be shared with clients for
// them to trust the server) and key (which should not be shared).
// All three will be zero values if the returned error is not nil.
func NewServerCert() (certBytes, keyBytes []byte, err error) {
	return newCert(false)
}

// NewClientCert is like NewServerCert, but it produces a certificate that
// is intended for client authentication.
func NewClientCert() (certBytes, keyBytes []byte, err error) {
	return newCert(true)
}

func newCert(isClientCert bool) ([]byte, []byte, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generated RSA key: %w", err)
	}
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate serial number: %w", err)
	}
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"ConnectRPC"},
		},
		NotBefore: time.Now().Add(-time.Hour * 24),
		NotAfter:  time.Now().Add(time.Hour * 24 * 7),

		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		BasicConstraintsValid: true,
	}
	if isClientCert {
		template.Subject.CommonName = ClientCertName
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	} else {
		template.Subject.CommonName = ServerCertName
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		template.IPAddresses = []net.IP{net.IPv6loopback, net.IPv4(127, 0, 0, 1)}
		template.DNSNames = []string{"localhost"}
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create certificate: %w", err)
	}

	certBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	return certBytes, keyBytes, nil
}
