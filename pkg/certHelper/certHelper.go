package certHelper

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// CreateDynamicTlsCertWithKey creates a fresh tls cert with private key for the passed SubjectName
func CreateDynamicTlsCertWithKey(subjectname string) (*tls.Certificate, error) {
	log.Infof("CreateDynamicTlsCertWithKey: creating new tls cert for SN: [%s]", subjectname)

	max := new(big.Int).Lsh(big.NewInt(1), 256)
	sn, err := rand.Int(rand.Reader, max)

	if err != nil {
		return nil, errors.New("failed to generate serial number: " + err.Error())
	}

	template := x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: subjectname},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(2, 0, 0),
		SignatureAlgorithm:    x509.SHA256WithRSA,
		BasicConstraintsValid: true,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth,
			x509.ExtKeyUsageServerAuth},
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:     true,
		DNSNames: []string{subjectname},
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return nil, errors.Wrap(err, "CreateDynamicTlsCertWithKey:rsa.GenerateKey")
	}

	derCert, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, errors.Wrap(err, "CreateDynamicTlsCertWithKey:x509.CreateCertificate")
	}

	//cert, err := x509.ParseCertificate(derCert)
	//if err != nil {
	//	return
	//}

	// PEM encode the certificate (this is a standard TLS encoding)
	b := pem.Block{Type: "CERTIFICATE", Bytes: derCert}
	certPEM := pem.EncodeToMemory(&b)

	// PEM encode the private key
	b = pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)}
	rootKeyPEM := pem.EncodeToMemory(&b)

	// Create a TLS cert using the private key and certificate
	tlsCertWithKey, err := tls.X509KeyPair(certPEM, rootKeyPEM)
	if err != nil {
		return nil, err
	}

	return &tlsCertWithKey, nil
}
