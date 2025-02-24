/*
Copyright 2020 The cert-manager Authors.

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

package certificates

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	fakeclock "k8s.io/utils/clock/testing"

	cmapi "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/jetstack/cert-manager/pkg/apis/meta/v1"
	controllerpkg "github.com/jetstack/cert-manager/pkg/controller"
	controllermetrics "github.com/jetstack/cert-manager/pkg/controller/certificates/metrics"
	logf "github.com/jetstack/cert-manager/pkg/logs"
	"github.com/jetstack/cert-manager/pkg/metrics"
	"github.com/jetstack/cert-manager/test/integration/framework"
	"github.com/jetstack/cert-manager/test/unit/gen"
)

var (
	fixedClock  = fakeclock.NewFakeClock(time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC))
	clockMetric = fmt.Sprintf(`# HELP certmanager_clock_time_seconds The clock time given in seconds (from 1970/01/01 UTC).
# TYPE certmanager_clock_time_seconds counter
certmanager_clock_time_seconds %.9e`, float64(fixedClock.Now().Unix()))
)

// TestMetricscontoller performs a basic test to ensure that Certificates
// metrics are exposed when a Certificate is created, updated, and removed when
// it is deleted.
func TestMetricsController(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*40)
	defer cancel()

	config, stopFn := framework.RunControlPlane(t, ctx)
	defer stopFn()

	// Build, instantiate and run the issuing controller.
	kubernetesCl, factory, cmClient, cmFactory := framework.NewClients(t, config)

	metricsHandler := metrics.New(logf.Log, fixedClock)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := metricsHandler.NewServer(ln, false)

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		if err := server.Serve(ln); err != http.ErrServerClosed {
			t.Fatal(err)
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second*5)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			t.Fatal(err)
		}
		<-doneCh
	}()

	ctrl, queue, mustSync := controllermetrics.NewController(factory, cmFactory, metricsHandler)
	c := controllerpkg.NewController(
		ctx,
		"metrics_test",
		metricsHandler,
		ctrl.ProcessItem,
		mustSync,
		nil,
		queue,
	)
	stopController := framework.StartInformersAndController(t, factory, cmFactory, c)
	defer stopController()

	var (
		crtName         = "testcrt"
		namespace       = "testns"
		metricsEndpoint = fmt.Sprintf("http://%s/metrics", server.Addr)

		lastErr error
	)

	// Create Namespace
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	_, err = kubernetesCl.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	testMetrics := func(expectedOutput string) error {
		resp, err := http.DefaultClient.Get(metricsEndpoint)
		if err != nil {
			return err
		}

		output, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}

		if strings.TrimSpace(string(output)) != strings.TrimSpace(expectedOutput) {
			return fmt.Errorf("got unexpected metrics output\nexp:\n%s\ngot:\n%s\n",
				expectedOutput, output)
		}

		return nil
	}

	waitForMetrics := func(expectedOutput string) {
		err := wait.PollImmediateUntil(time.Millisecond*100, func() (done bool, err error) {
			if err := testMetrics(expectedOutput); err != nil {
				lastErr = err
				return false, nil
			}

			return true, nil
		}, ctx.Done())
		if err != nil {
			t.Fatalf("%s: failed to wait for expected metrics to be exposed: %s", err, lastErr)
		}
	}

	// Should expose no additional metrics
	waitForMetrics(clockMetric)

	// Create Certificate
	crt := gen.Certificate(crtName,
		gen.SetCertificateIssuer(cmmeta.ObjectReference{Kind: "Issuer", Name: "test-issuer"}),
		gen.SetCertificateSecretName(crtName),
		gen.SetCertificateCommonName(crtName),
		gen.SetCertificateNamespace(namespace),
		gen.SetCertificateUID("uid-1"),
	)

	crt, err = cmClient.CertmanagerV1().Certificates(namespace).Create(ctx, crt, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Should expose that Certificate as unknown with no expiry
	waitForMetrics(`# HELP certmanager_certificate_expiration_timestamp_seconds The date after which the certificate expires. Expressed as a Unix Epoch Time.
# TYPE certmanager_certificate_expiration_timestamp_seconds gauge
certmanager_certificate_expiration_timestamp_seconds{name="testcrt",namespace="testns"} 0
# HELP certmanager_certificate_ready_status The ready status of the certificate.
# TYPE certmanager_certificate_ready_status gauge
certmanager_certificate_ready_status{condition="False",name="testcrt",namespace="testns"} 0
certmanager_certificate_ready_status{condition="True",name="testcrt",namespace="testns"} 0
certmanager_certificate_ready_status{condition="Unknown",name="testcrt",namespace="testns"} 1
` + clockMetric + `
# HELP certmanager_controller_sync_call_count The number of sync() calls made by a controller.
# TYPE certmanager_controller_sync_call_count counter
certmanager_controller_sync_call_count{controller="metrics_test"} 1
`)

	// Set Certificate Expiry and Ready status True
	crt.Status.NotAfter = &metav1.Time{
		Time: time.Unix(100, 0),
	}
	crt.Status.Conditions = []cmapi.CertificateCondition{
		{
			Type:   cmapi.CertificateConditionReady,
			Status: cmmeta.ConditionTrue,
		},
	}
	_, err = cmClient.CertmanagerV1().Certificates(namespace).UpdateStatus(ctx, crt, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Should expose that Certificate as ready with expiry
	waitForMetrics(`# HELP certmanager_certificate_expiration_timestamp_seconds The date after which the certificate expires. Expressed as a Unix Epoch Time.
# TYPE certmanager_certificate_expiration_timestamp_seconds gauge
certmanager_certificate_expiration_timestamp_seconds{name="testcrt",namespace="testns"} 100
# HELP certmanager_certificate_ready_status The ready status of the certificate.
# TYPE certmanager_certificate_ready_status gauge
certmanager_certificate_ready_status{condition="False",name="testcrt",namespace="testns"} 0
certmanager_certificate_ready_status{condition="True",name="testcrt",namespace="testns"} 1
certmanager_certificate_ready_status{condition="Unknown",name="testcrt",namespace="testns"} 0
` + clockMetric + `
# HELP certmanager_controller_sync_call_count The number of sync() calls made by a controller.
# TYPE certmanager_controller_sync_call_count counter
certmanager_controller_sync_call_count{controller="metrics_test"} 2
`)

	err = cmClient.CertmanagerV1().Certificates(namespace).Delete(ctx, crt.Name, metav1.DeleteOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Should expose no Certificates and only metrics sync count increase
	waitForMetrics(clockMetric + `
# HELP certmanager_controller_sync_call_count The number of sync() calls made by a controller.
# TYPE certmanager_controller_sync_call_count counter
certmanager_controller_sync_call_count{controller="metrics_test"} 3
`)
}
