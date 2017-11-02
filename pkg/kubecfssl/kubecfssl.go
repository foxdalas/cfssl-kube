package kubecfssl

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"github.com/foxdalas/cfssl-kube/pkg/kubecfssl_const"
	"github.com/foxdalas/cfssl-kube/pkg/cfssl"
	"github.com/foxdalas/cfssl-kube/pkg/secret"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

var _ kubecfssl.KubeCfssl = &KubeCfssl{}

func New(version string) *KubeCfssl {
	return &KubeCfssl{
		version:   version,
		log:       makeLog(),
		stopCh:    make(chan struct{}),
		waitGroup: sync.WaitGroup{},
	}
}

func (kc *KubeCfssl) Log() *log.Entry {
	return kc.log
}

func (kc *KubeCfssl) Init() {
	kc.Log().Infof("cfkube %s starting", kc.version)

	// handle sigterm correctly
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-c
		logger := kc.Log().WithField("signal", s.String())
		logger.Debug("received signal")
		kc.Stop()
	}()

	// parse env vars
	err := kc.paramsCF()
	if err != nil {
		kc.Log().Fatal(err)
	}

	err = kc.InitKube()
	if err != nil {
		kc.Log().Fatal(err)
	}

	kc.Log().Infoln("Periodically check start")
	ticker := time.NewTicker(kc.checkInterval)
	cs := cfssl.New(kc)
	go func() {
		timestamp := time.Now()
			kc.Log().Infof("Periodically check certificates at %s", timestamp)
			for _, namespace := range kc.kubeNamespaces {
				kc.namespace = namespace
				kc.secretName = "cfssl-tls-secret"
				kc.Log().Infoln("Checking namespace:", kc.namespace)

				s := secret.New(kc, namespace, "cfssl-tls-secret")

				s.SecretApi.Name = "cfssl-tls-secret"
				s.SecretApi.Namespace = namespace

				if !s.Exists() {
					kc.Log().Printf("Secret for namespace %s is not exist", kc.namespace)
					kc.SaveSecret(cs.GetCertificate(kc.address, kc.authKey, kc.csrConfig, cs.CreateKey()))
				} else {
					kc.Log().Printf("Secret for namespace %s already exist", kc.namespace)
					validate := kc.ValidateTLS(s.SecretApi.Data["ca.pem"], s.SecretApi.Data["crt.pem"], s.SecretApi.Data["crt.key"])
					if !validate {
						//kc.SaveSecret(cs.GetCertificate(kc.cfAddress, kc.cfAuthKey, kc.cfCSRConfig, cs.CreateKey()))
						kc.Log().Println("Certificate validation problem.")
					}
				}
			}
		<- ticker.C
	}()

	<-kc.stopCh
	ticker.Stop()
	kc.Log().Infof("exiting")
	kc.waitGroup.Wait()

}

func makeLog() *log.Entry {
	logtype := strings.ToLower(os.Getenv("CFKUBE_LOG_TYPE"))
	if logtype == "" {
		logtype = "text"
	}

	if logtype == "json" {
		log.SetFormatter(&log.JSONFormatter{})
	} else if logtype == "text" {
		log.SetFormatter(&log.TextFormatter{})
	} else {
		log.WithField("logtype", logtype).Fatal("Given logtype was not valid, check CFKUBELOG_TYPE configuration")
		os.Exit(1)
	}

	loglevel := strings.ToLower(os.Getenv("LEGO_LOG_LEVEL"))
	if len(loglevel) == 0 {
		log.SetLevel(log.InfoLevel)
	} else if loglevel == "debug" {
		log.SetLevel(log.DebugLevel)
	} else if loglevel == "info" {
		log.SetLevel(log.InfoLevel)
	} else if loglevel == "warn" {
		log.SetLevel(log.WarnLevel)
	} else if loglevel == "error" {
		log.SetLevel(log.ErrorLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	return log.WithField("context", "cfkube")
}

func (kc *KubeCfssl) Version() string {
	return kc.version
}

func (kc *KubeCfssl) Stop() {
	kc.Log().Info("shutting things down")
	close(kc.stopCh)
}

func (kc *KubeCfssl) CFNamespace() string {
	return kc.namespace
}

func (kc *KubeCfssl) KubeCheckInterval() time.Duration {
	return kc.checkInterval
}

func (kc *KubeCfssl) KubeApiURL() string {
	return kc.kubeApiURL
}

func (kc *KubeCfssl) KubeClient() *kubernetes.Clientset {
	return kc.kubeClient
}

func (kc *KubeCfssl) cfsslSecret() *secret.Secret {
	return secret.New(kc, kc.namespace, kc.secretName)
}

func (kc *KubeCfssl) paramsCF() error {

	kc.address = os.Getenv("CFKUBE_CFSSL_ADDRESS")
	if len(kc.address) == 0 {
		return errors.New("Please provide an address for CFSSL Server in CFKUBE_CFSSL_ADDRESS")
	}

	kc.authKey = os.Getenv("CFKUBE_CFSSL_AUTH_KEY")
	if len(kc.authKey) == 0 {
		return errors.New("Please provide the secret key via environment variable CFKUBE_CFSSL_AUTH_KEY ")
	}

	checkIntervalString := os.Getenv("CFKUBE_CHECK_INTERVAL")
	if len(checkIntervalString) == 0 {
		kc.checkInterval = 1 * time.Minute
	}
	kc.csrConfig = []byte(os.Getenv("CFKUBE_CFSSL_CSR"))
	if len(kc.csrConfig) == 0 {
		return errors.New("Please provide the secret key via environment variable CFKUBE_CFSSL_CSR ")
	}

	kc.kubeApiURL = os.Getenv("CFKUBE_KUBE_API_URL")
	if len(kc.kubeApiURL) == 0 {
		kc.kubeApiURL = "http://127.0.0.1:8080"
	}

	kc.kubeNamespaces = strings.Split(os.Getenv("CFKUBE_NAMESPACES"), ",")
	if len(kc.kubeNamespaces) == 0 {
		return errors.New("Please provide the namespaces via environment variable CFKUBE_NAMESPACES (default,test,production)")
	}

	return nil
}

func (kc *KubeCfssl) SaveSecret(data map[string][]byte) error {
	s := kc.cfsslSecret()
	s.SecretApi.Data = data
	return s.Save()
}

func (c *KubeCfssl) ValidateTLS(caByte []byte, certByte []byte, keyByte []byte) bool {

	block, _ := pem.Decode(certByte)

	if block == nil {
		c.Log().Errorln("Failed to parse certificate PEM")
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		c.Log().Printf("Failed to parse certificate: %s", err)
		return false
	}
	if (cert.NotAfter.Unix() - time.Now().Unix()) < int64(kubecfssl.ExpireThreshold) {
		c.Log().Warningf("Certificate expire date > Threshold ")
		return false
	} else {
		c.Log().Infoln("Certificate expire date is OK")
	}

	_, err = tls.X509KeyPair(certByte, keyByte)
	if err != nil {
		c.Log().Warningln("Certificate cert/key is mismatch")
		return false
	} else {
		c.Log().Infoln("Certificate cert/key is OK")
	}

	roots := x509.NewCertPool()
	ok := roots.AppendCertsFromPEM(caByte)
	if !ok {
		log.Warnln("Failed to parse root certificate")
		return false
	}

	for _, dnsName := range cert.DNSNames {
		opts := x509.VerifyOptions{
			DNSName: dnsName,
			Roots:   roots,
		}
		c.Log().Infof("Validating certificate for DNS name: %s", dnsName)
		if _, err := cert.Verify(opts); err != nil {
			c.Log().Warnf("failed to verify certificate: " + err.Error())
			return false
		} else {
			c.Log().Infof("Certificate is valid for %s", dnsName)
		}
	}

	return true
}