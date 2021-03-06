/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/golang/glog"

	api "k8s.io/client-go/pkg/api/v1"
	extensions "k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/tools/cache"

	"k8s.io/ingress/core/pkg/ingress"
	"k8s.io/ingress/core/pkg/ingress/annotations/parser"
	ssl "k8s.io/ingress/core/pkg/net/ssl"
)

// syncSecret keeps in sync Secrets used by Ingress rules with the files on
// disk to allow being used in controllers.
func (ic *GenericController) syncSecret(k interface{}) error {
	if ic.secretQueue.IsShuttingDown() {
		return nil
	}
	if !ic.controllersInSync() {
		time.Sleep(podStoreSyncedPollPeriod)
		return fmt.Errorf("deferring sync till endpoints controller has synced")
	}

	var key string
	var cert *ingress.SSLCert
	var err error

	key = k.(string)

	secObj, exists, err := ic.secrLister.Store.GetByKey(key)
	if err != nil {
		return fmt.Errorf("error getting secret %v: %v", key, err)
	}
	if !exists {
		return fmt.Errorf("secret %v was not found", key)
	}
	sec := secObj.(*api.Secret)
	if !ic.secrReferenced(sec.Name, sec.Namespace) {
		glog.V(3).Infof("secret %v/%v is not used in Ingress rules. skipping ", sec.Namespace, sec.Name)
		return nil
	}

	cert, err = ic.getPemCertificate(key)
	if err != nil {
		return err
	}

	// create certificates and add or update the item in the store
	cur, exists := ic.sslCertTracker.Get(key)
	if exists {
		s := cur.(*ingress.SSLCert)
		if reflect.DeepEqual(s, cert) {
			// no need to update
			return nil
		}
		glog.Infof("updating secret %v/%v in the local store", sec.Namespace, sec.Name)
		ic.sslCertTracker.Update(key, cert)
		return nil
	}
	glog.Infof("adding secret %v/%v to the local store", sec.Namespace, sec.Name)
	ic.sslCertTracker.Add(key, cert)
	return nil
}

// getPemCertificate receives a secret, and creates a ingress.SSLCert as return.
// It parses the secret and verifies if it's a keypair, or a 'ca.crt' secret only.
func (ic *GenericController) getPemCertificate(secretName string) (*ingress.SSLCert, error) {
	secretInterface, exists, err := ic.secrLister.Store.GetByKey(secretName)
	if err != nil {
		return nil, fmt.Errorf("error retrieving secret %v: %v", secretName, err)
	}
	if !exists {
		return nil, fmt.Errorf("secret named %v does not exist", secretName)
	}

	secret := secretInterface.(*api.Secret)
	cert, okcert := secret.Data[api.TLSCertKey]
	key, okkey := secret.Data[api.TLSPrivateKeyKey]

	ca := secret.Data["ca.crt"]

	nsSecName := strings.Replace(secretName, "/", "-", -1)

	var s *ingress.SSLCert
	if okcert && okkey {
		glog.Infof("found certificate and private key, configuring %v as a TLS Secret", secretName)
		s, err = ssl.AddOrUpdateCertAndKey(nsSecName, cert, key, ca)
	} else if ca != nil {
		glog.Infof("found only ca.crt, configuring %v as an Certificate Authentication secret", secretName)
		s, err = ssl.AddCertAuth(nsSecName, ca)
	} else {
		return nil, fmt.Errorf("no keypair or CA cert could be found in %v", secretName)
	}

	if err != nil {
		return nil, err
	}

	s.Name = secret.Name
	s.Namespace = secret.Namespace
	return s, nil
}

// secrReferenced checks if a secret is referenced or not by one or more Ingress rules
func (ic *GenericController) secrReferenced(name, namespace string) bool {
	for _, ingIf := range ic.ingLister.Store.List() {
		ing := ingIf.(*extensions.Ingress)

		if ic.annotations.ContainsCertificateAuth(ing) {
			str, _ := parser.GetStringAnnotation("ingress.kubernetes.io/auth-tls-secret", ing)
			if str == fmt.Sprintf("%v/%v", namespace, name) {
				return true
			}
		}

		if ing.Namespace != namespace {
			continue
		}
		for _, tls := range ing.Spec.TLS {
			if tls.SecretName == name {
				return true
			}
		}
	}
	return false
}

// sslCertTracker holds a store of referenced Secrets in Ingress rules
type sslCertTracker struct {
	cache.ThreadSafeStore
}

func newSSLCertTracker() *sslCertTracker {
	return &sslCertTracker{
		cache.NewThreadSafeStore(cache.Indexers{}, cache.Indices{}),
	}
}
