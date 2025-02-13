// Copyright 2022 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package registration

import (
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"time"

	certv1 "k8s.io/api/certificates/v1"
	"k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/certificate/csr"
	"k8s.io/client-go/util/keyutil"
	"k8s.io/klog/v2"
)

const (
	KeySize = 2048
	// ExpirationSeconds defines the expiry time for Certificates
	// which is currently set to 1 year aligned with kubeadm defaults.
	ExpirationSeconds = 86400 * 365
	ByohCSROrg        = "byoh:hosts"
	ByohCSRCNFormat   = "byoh:host:%s"
	ByohCSRNameFormat = "byoh-csr-%s"
	// CSRApprovalTimeout defines the time to wait for certificate to
	// be issued. Currently set to 1 hour.
	CSRApprovalTimeout = 3600 * time.Second
	TmpPrivateKey      = "byoh-client.key.tmp"
)

type ByohCSR struct {
	BootstrapClient clientset.Interface
	PrivateKey      []byte
}

// RequestBYOHClientCert will generate Private Key and then will create a
// CertificateSigningRequest in K8s
func (bcsr *ByohCSR) RequestBYOHClientCert(hostname string) (string, types.UID, error) {
	if hostname == "" {
		return "", "", fmt.Errorf("hostname is not valid")
	}
	keyData, _, err := keyutil.LoadOrGenerateKeyFile(TmpPrivateKey)
	if err != nil {
		return "", "", err
	}
	privateKey, err := keyutil.ParsePrivateKeyPEM(keyData)
	if err != nil {
		return "", "", fmt.Errorf("invalid private key for certificate request: %v", err)
	}
	bcsr.PrivateKey = keyData
	csrData, err := generateCSR(hostname, privateKey)
	if err != nil {
		klog.Errorf("error generating csr %s, err=%v", hostname, err)
		return "", "", err
	}
	certTimeToExpire := time.Duration(ExpirationSeconds) * time.Second
	reqName, reqUID, err := csr.RequestCertificate(bcsr.BootstrapClient,
		csrData,
		fmt.Sprintf(ByohCSRNameFormat, hostname),
		certv1.KubeAPIServerClientSignerName,
		&certTimeToExpire,
		[]certv1.KeyUsage{certv1.UsageClientAuth},
		privateKey)
	if err != nil {
		return "", "", err
	}
	return reqName, reqUID, nil
}

func generateCSR(hostname string, privKey interface{}) ([]byte, error) {
	// Generate a new *x509.CertificateRequest template
	csrTemplate := x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   fmt.Sprintf(ByohCSRCNFormat, hostname),
			Organization: []string{ByohCSROrg},
		},
	}
	// Generate the CSR bytes
	csrData, err := x509.CreateCertificateRequest(rand.Reader, &csrTemplate, privKey)
	if err != nil {
		return nil, err
	}
	csrPemBlock := &pem.Block{
		Type:  cert.CertificateRequestBlockType,
		Bytes: csrData,
	}
	return pem.EncodeToMemory(csrPemBlock), nil
}

// LoadRESTClientConfig is to create an instance of *restclient.Config from
// the boostrap kubeconfig path, this then will be used to create bootstrap
// k8s client
func LoadRESTClientConfig(bootstrapKubeconfig string) (*restclient.Config, error) {
	loader := &clientcmd.ClientConfigLoadingRules{ExplicitPath: bootstrapKubeconfig}
	loadedConfig, err := loader.Load()
	if err != nil {
		return nil, err
	}
	// Flatten the loaded data to a particular restclient.Config based on the current context.
	return clientcmd.NewNonInteractiveClientConfig(
		*loadedConfig,
		loadedConfig.CurrentContext,
		&clientcmd.ConfigOverrides{},
		loader,
	).ClientConfig()
}

// WriteKubeconfigFromBootstrapping will write the new kubeconfig fetching
// some details from bootstrap client config and using key/cert details
func WriteKubeconfigFromBootstrapping(bootstrapClientConfig *restclient.Config, kubeconfigPath, certData, keyData string) error {
	// Get the CA data from the bootstrap client config.
	caFile, caData := bootstrapClientConfig.CAFile, []byte{}
	if caFile == "" {
		caData = bootstrapClientConfig.CAData
	}

	// Build resulting kubeconfig.
	kubeconfigData := clientcmdapi.Config{
		// Define a cluster stanza based on the bootstrap kubeconfig.
		Clusters: map[string]*clientcmdapi.Cluster{"default-cluster": {
			Server:                   bootstrapClientConfig.Host,
			InsecureSkipTLSVerify:    bootstrapClientConfig.Insecure,
			CertificateAuthority:     caFile,
			CertificateAuthorityData: caData,
		}},
		// Define auth based on the obtained client cert.
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"default-auth": {
			ClientCertificate: certData,
			ClientKey:         keyData,
		}},
		// Define a context that connects the auth info and cluster, and set it as the default
		Contexts: map[string]*clientcmdapi.Context{"default-context": {
			Cluster:   "default-cluster",
			AuthInfo:  "default-auth",
			Namespace: "default",
		}},
		CurrentContext: "default-context",
	}

	// Marshal to disk
	return clientcmd.WriteToFile(kubeconfigData, kubeconfigPath)
}
