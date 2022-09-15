package controllers

import (
	"bytes"
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	slug "github.com/hashicorp/go-slug"
	tfc "github.com/hashicorp/go-tfe"
	appv1alpha2 "github.com/hashicorp/terraform-cloud-operator/api/v1alpha2"
)

var _ = Describe("Workspace controller", Ordered, func() {
	var (
		instance  *appv1alpha2.Workspace
		workspace = fmt.Sprintf("kubernetes-operator-%v", GinkgoRandomSeed())
	)

	BeforeAll(func() {
		// Set default Eventually timers
		SetDefaultEventuallyTimeout(90 * time.Second)
		SetDefaultEventuallyPollingInterval(2 * time.Second)
	})

	BeforeEach(func() {
		// Create a new workspace object for each test
		instance = &appv1alpha2.Workspace{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "app.terraform.io/v1alpha2",
				Kind:       "Workspace",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:              namespacedName.Name,
				Namespace:         namespacedName.Namespace,
				DeletionTimestamp: nil,
				Finalizers:        []string{},
			},
			Spec: appv1alpha2.WorkspaceSpec{
				Organization: organization,
				Token: appv1alpha2.Token{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: namespacedName.Name,
						},
						Key: secretKey,
					},
				},
				Name: workspace,
			},
			Status: appv1alpha2.WorkspaceStatus{},
		}
	})

	AfterEach(func() {
		// Delete the Kubernetes workspace object and wait until the controller finishes the reconciliation after deletion of the object
		deleteWorkspace(instance, namespacedName)
	})

	Context("Workspace controller", func() {
		It("can handle outputs", func() {
			// Create a new Kubernetes workspace object and wait until the controller finishes the reconciliation
			createWorkspace(instance, namespacedName)

			// Create a temporary dir in the current one
			cd, err := os.Getwd()
			Expect(err).Should(Succeed())
			td, err := os.MkdirTemp(cd, "tf-*")
			Expect(err).Should(Succeed())
			defer os.Remove(td)
			// Create a temporary file in the temporary dir
			f, err := os.CreateTemp(td, "*.tf")
			Expect(err).Should(Succeed())
			defer os.Remove(f.Name())
			// Terraform code to upload
			o := "hoi"
			tf := fmt.Sprintf(`
			output "sensitive" {
				value = "%s"
				sensitive = true
			}
			output "non_sensitive" {
				value = "%s"
			}`, o, o)
			// Save the Terraform code to the temporary file
			_, err = f.WriteString(tf)
			Expect(err).Should(Succeed())
			// Make a new slug
			b := bytes.NewBuffer(nil)
			_, err = slug.Pack(td, b, false)
			Expect(err).Should(Succeed())
			// Create a temporry archive file to store the slug
			tgz, err := os.CreateTemp(td, "*.tar.gz")
			Expect(err).Should(Succeed())
			defer os.Remove(tgz.Name())
			// Write slug to the archive
			_, err = tgz.Write(b.Bytes())
			Expect(err).Should(Succeed())

			// Enable AutoApply to automatically apply the code that will be uploaded
			_, err = tfClient.Workspaces.UpdateByID(ctx, instance.Status.WorkspaceID, tfc.WorkspaceUpdateOptions{AutoApply: tfc.Bool(true)})
			Expect(err).Should(Succeed())

			cv, err := tfClient.ConfigurationVersions.Create(ctx, instance.Status.WorkspaceID, tfc.ConfigurationVersionCreateOptions{})
			Expect(cv).ShouldNot(BeNil())
			Expect(err).Should(Succeed())

			Expect(tfClient.ConfigurationVersions.Upload(ctx, cv.UploadURL, td)).Should(Succeed())

			By("Validating configuration version successful upload")
			Eventually(func() bool {
				cv, err := tfClient.ConfigurationVersions.Read(ctx, cv.ID)
				if err != nil {
					return false
				}
				if cv.Status == tfc.ConfigurationUploaded {
					return true
				}
				return false
			}).Should(BeTrue())

			By("Validating configuration version and workspace run")
			Eventually(func() bool {
				Expect(k8sClient.Get(ctx, namespacedName, instance)).Should(Succeed())
				runs, err := tfClient.Runs.List(ctx, instance.Status.WorkspaceID, &tfc.RunListOptions{})
				Expect(runs).ShouldNot(BeNil())
				Expect(err).Should(Succeed())

				for _, r := range runs.Items {
					if r.ConfigurationVersion.ID == cv.ID && r.ID == instance.Status.Run.OutputRunID {
						return true
					}
				}
				return false
			}).Should(BeTrue())

			outputsNamespacedName := types.NamespacedName{
				Name:      outputObjectName(namespacedName.Name),
				Namespace: namespacedName.Namespace,
			}

			s := &corev1.Secret{}
			By("Validating sensitive outputs")
			Eventually(func() bool {
				Expect(k8sClient.Get(ctx, outputsNamespacedName, s)).Should(Succeed())
				if v, ok := s.Data["sensitive"]; ok {
					if string(v) == o {
						return true
					}
				}
				return false
			}).Should(BeTrue())

			cm := &corev1.ConfigMap{}
			By("Validating non-sensitive outputs")
			Eventually(func() bool {
				Expect(k8sClient.Get(ctx, outputsNamespacedName, cm)).Should(Succeed())
				if v, ok := cm.Data["non_sensitive"]; ok {
					if v == o {
						return true
					}
				}
				return false
			}).Should(BeTrue())
		})
	})
})
