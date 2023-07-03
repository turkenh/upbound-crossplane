/*
Copyright 2023 The Crossplane Authors.
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
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/vladimirvivien/gexe"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	cr "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/third_party/helm"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"

	v1 "github.com/crossplane/crossplane/apis/apiextensions/v1"
	"github.com/crossplane/crossplane/test/e2e/funcs"
)

func TestXfnRunnerImagePull(t *testing.T) {

	manifests := "test/e2e/manifests/xfnrunner/private-registry/pull"
	environment.Test(t,
		features.New("PullFnImageFromPrivateRegistryWithCustomCert").
			WithLabel(LabelArea, "xfn").
			WithSetup("InstallRegistryWithCustomTlsCertificate",
				funcs.AllOf(
					funcs.AsFeaturesFunc(envfuncs.CreateNamespace("reg")),
					CreateTLSCertificateAsSecret("private-docker-registry.reg.svc.cluster.local", "reg"),
					InstallDockerRegistry(),
				)).
			WithSetup("CopyFnImageToRegistry", CopyImagesToRegistry()).
			WithSetup("CrossplaneDeployedWithFunctionsEnabled", CrossplaneDeployedWithFunctionsEnabled()).
			WithSetup("ProvideNopDeployed", ProvideNopDeployed()).
			Assess("CompositionWithFunctionIsCreated", funcs.AllOf(
				funcs.ApplyResources(FieldManager, manifests, "composition.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, manifests, "composition.yaml"),
			)).
			Assess("ClaimIsCreated", funcs.AllOf(
				funcs.ApplyResources(FieldManager, manifests, "claim.yaml"),
				funcs.ResourcesCreatedWithin(30*time.Second, manifests, "claim.yaml"),
			)).
			Assess("ClaimBecomesAvailable", funcs.ResourcesHaveConditionWithin(5*time.Minute, manifests, "claim.yaml", xpv1.Available())).
			Assess("ManagedResourcesProcessedByFunction", ManagedResourcedProcessedByFunction()).
			Feature(),
	)
}

func resourceGetter(ctx context.Context, t *testing.T, config *envconf.Config) func(string, string, string, string) *unstructured.Unstructured {
	return func(name string, namespace string, apiVersion string, kind string) *unstructured.Unstructured {
		client := config.Client().Resources().GetControllerRuntimeClient()
		u := &unstructured.Unstructured{}
		gv, err := schema.ParseGroupVersion(apiVersion)
		if err != nil {
			t.Fatal(err)
		}
		u.SetGroupVersionKind(gv.WithKind(kind))
		if err := client.Get(ctx, cr.ObjectKey{Name: name, Namespace: namespace}, u); err != nil {
			t.Fatal("cannot get claim", err)
		}
		return u
	}
}
func resourceValue(t *testing.T, u *unstructured.Unstructured, path ...string) map[string]string {
	f, found, err := unstructured.NestedStringMap(u.Object, path...)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("field not found at path %v", path)
	}
	return f
}

func resourceSliceValue(t *testing.T, u *unstructured.Unstructured, path ...string) []map[string]string {
	f, found, err := unstructured.NestedSlice(u.Object, path...)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("field not found at path %v", path)
	}
	var s []map[string]string
	for _, v := range f {
		if vv, ok := v.(map[string]interface{}); ok {
			s = append(s, asMapOfStrings(vv))
		} else {
			t.Fatalf("not a map[string]string: %v type %s", v, reflect.TypeOf(v))
		}
	}
	return s
}

func asMapOfStrings(m map[string]interface{}) map[string]string {
	r := make(map[string]string)
	for k, v := range m {
		r[k] = fmt.Sprintf("%v", v)
	}
	return r
}

// ManagedResourcedProcessedByFunction asserts that MRs contains the requested label
func ManagedResourcedProcessedByFunction() features.Func {

	return func(ctx context.Context, t *testing.T, config *envconf.Config) context.Context {
		labelName := "labelizer.xfn.crossplane.io/processed"
		rg := resourceGetter(ctx, t, config)
		claim := rg("fn-labelizer", "default", "nop.example.org/v1alpha1", "NopResource")
		r := resourceValue(t, claim, "spec", "resourceRef")

		xr := rg(r["name"], "default", r["apiVersion"], r["kind"])
		mrefs := resourceSliceValue(t, xr, "spec", "resourceRefs")
		for _, mref := range mrefs {
			err := wait.For(func() (done bool, err error) {
				mr := rg(mref["name"], "default", mref["apiVersion"], mref["kind"])
				l, found := mr.GetLabels()[labelName]
				if !found {
					return false, nil
				}
				if l != "true" {
					return false, nil
				}
				return true, nil
			}, wait.WithTimeout(5*time.Minute))
			if err != nil {
				t.Fatalf("Expected label %v value to be true", labelName)
			}

		}
		return ctx
	}
}

// CreateTLSCertificateAsSecret for given dns name and store the secret in the given namespace
func CreateTLSCertificateAsSecret(dnsName string, ns string) features.Func {
	return func(ctx context.Context, t *testing.T, config *envconf.Config) context.Context {
		caPem, keyPem, err := createCert(dnsName)
		if err != nil {
			t.Fatal(err)
		}

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "reg-cert",
				Namespace: ns,
			},
			Type: corev1.SecretTypeTLS,
			StringData: map[string]string{
				"tls.crt": caPem,
				"tls.key": keyPem,
			},
		}
		client := config.Client().Resources()
		if err := client.Create(ctx, secret); err != nil {
			t.Fatalf("Cannot create secret %s: %v", secret.Name, err)
		}
		configMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "reg-ca",
				Namespace: namespace,
			},
			Data: map[string]string{
				"domain.crt": caPem,
			},
		}
		if err := client.Create(ctx, configMap); err != nil {
			t.Fatalf("Cannot create config %s: %v", configMap.Name, err)
		}
		return ctx
	}
}

// CopyImagesToRegistry copies fn images to private registry
func CopyImagesToRegistry() features.Func {
	return func(ctx context.Context, t *testing.T, config *envconf.Config) context.Context {
		nodes := &corev1.NodeList{}
		if err := config.Client().Resources().List(ctx, nodes); err != nil {
			t.Fatal("cannot list nodes", err)
		}
		if len(nodes.Items) == 0 {
			t.Fatalf("no nodes in the cluster")
		}
		var addr string
		for _, a := range nodes.Items[0].Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				addr = a.Address
				break
			}
		}
		if addr == "" {
			t.Fatalf("no nodes with private address")
		}

		p := gexe.RunProc(fmt.Sprintf("skopeo copy docker-daemon:crossplane-e2e/fn-labelizer:latest docker://%s:32000/fn-labelizer:latest --dest-tls-verify=false", addr)).Wait()
		out, _ := io.ReadAll(p.Out())
		t.Logf("skopeo stdout: %s", string(out))
		if p.ExitCode() != 0 {
			t.Fatalf("copying image to registry not successful, exit code %v std out %v std err %v", p.ExitCode(), string(out), p.Err())
		}
		return ctx
	}
}

// InstallDockerRegistry with custom TLS
func InstallDockerRegistry() features.Func {
	return funcs.AllOf(
		funcs.AsFeaturesFunc(
			funcs.HelmRepo(
				helm.WithArgs("add"),
				helm.WithArgs("twuni"),
				helm.WithArgs("https://helm.twun.io"),
			)),
		funcs.AsFeaturesFunc(
			funcs.HelmInstall(
				helm.WithName("private"),
				helm.WithNamespace("reg"),
				helm.WithWait(),
				helm.WithChart("twuni/docker-registry"),
				helm.WithVersion("2.2.2"),
				helm.WithArgs(
					"--set service.type=NodePort",
					"--set service.nodePort=32000",
					"--set tlsSecretName=reg-cert",
				),
			)))
}

// CrossplaneDeployedWithFunctionsEnabled asserts that crossplane deployment with composition functions is enabled
func CrossplaneDeployedWithFunctionsEnabled() features.Func {
	return funcs.AllOf(
		funcs.AsFeaturesFunc(funcs.HelmUpgrade(
			HelmOptions(
				helm.WithArgs(
					"--set args={--debug,--enable-composition-functions}",
					"--set xfn.enabled=true",
					"--set xfn.args={--debug}",
					"--set registryCaBundleConfig.name=reg-ca",
					"--set registryCaBundleConfig.key=domain.crt",
					"--set xfn.resources.requests.cpu=100m",
					"--set xfn.resources.limits.cpu=100m",
				),
				helm.WithWait())...)),
		funcs.ReadyToTestWithin(1*time.Minute, namespace),
	)
}

// ProvideNopDeployed assets that provider-nop is deployed and healthy
func ProvideNopDeployed() features.Func {
	manifests := "test/e2e/manifests/xfnrunner/private-registry/pull/prerequisites"
	return funcs.AllOf(
		funcs.ApplyResources(FieldManager, manifests, "provider.yaml"),
		funcs.ApplyResources(FieldManager, manifests, "definition.yaml"),
		funcs.ResourcesCreatedWithin(30*time.Second, manifests, "provider.yaml"),
		funcs.ResourcesCreatedWithin(30*time.Second, manifests, "definition.yaml"),
		funcs.ResourcesHaveConditionWithin(1*time.Minute, manifests, "definition.yaml", v1.WatchingComposite()),
	)
}

func createCert(dnsName string) (string, string, error) {
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(2019),
		Subject: pkix.Name{
			Organization:  []string{"Company, INC."},
			Country:       []string{"US"},
			Province:      []string{""},
			Locality:      []string{""},
			StreetAddress: []string{""},
			PostalCode:    []string{""},
			CommonName:    dnsName,
		},
		DNSNames:              []string{dnsName},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	// create our private and public key
	caPrivKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return "", "", err
	}

	// create the CA
	caBytes, err := x509.CreateCertificate(rand.Reader, ca, ca, &caPrivKey.PublicKey, caPrivKey)
	if err != nil {
		return "", "", err
	}

	// pem encode
	caPEM := new(bytes.Buffer)
	pem.Encode(caPEM, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: caBytes,
	})

	keyPEM := new(bytes.Buffer)
	pem.Encode(keyPEM, &pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(caPrivKey),
	})

	return caPEM.String(), keyPEM.String(), nil
}
