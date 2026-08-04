package v1

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/url"
)

type ClientTLSOptions struct {
	Certificate       tls.Certificate
	RootCAs           *x509.CertPool
	ServerName        string
	ExpectedServerURI string
}

func NewClientTLSConfig(options ClientTLSOptions) (*tls.Config, error) {
	expected, err := url.Parse(options.ExpectedServerURI)
	if err != nil || expected.Scheme == "" || expected.Host == "" || options.RootCAs == nil ||
		options.ServerName == "" || len(options.Certificate.Certificate) == 0 {
		return nil, errors.New("private Runner client TLS configuration is invalid")
	}
	config := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{options.Certificate},
		RootCAs:      options.RootCAs,
		ServerName:   options.ServerName,
	}
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
			return ErrUnauthorized
		}
		leaf := state.PeerCertificates[0]
		if !hasExplicitUsage(leaf, x509.ExtKeyUsageServerAuth) || !hasExactURI(leaf, expected) {
			return ErrUnauthorized
		}
		return nil
	}
	return config, nil
}

func hasExactURI(certificate *x509.Certificate, expected *url.URL) bool {
	return certificate != nil && len(certificate.URIs) == 1 && certificate.URIs[0].String() == expected.String()
}

func hasExplicitUsage(certificate *x509.Certificate, expected x509.ExtKeyUsage) bool {
	if certificate == nil {
		return false
	}
	for _, usage := range certificate.ExtKeyUsage {
		if usage == x509.ExtKeyUsageAny {
			return false
		}
		if usage == expected {
			return true
		}
	}
	return false
}
