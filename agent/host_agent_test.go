// Copyright 2021 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// nolint: testpackage
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/jackpal/gateway"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
	"github.com/vmware-tanzu/cluster-api-provider-bringyourownhost/agent/registration"
	"github.com/vmware-tanzu/cluster-api-provider-bringyourownhost/agent/version"
	infrastructurev1beta1 "github.com/vmware-tanzu/cluster-api-provider-bringyourownhost/apis/infrastructure/v1beta1"
	"github.com/vmware-tanzu/cluster-api-provider-bringyourownhost/test/builder"
	"github.com/vmware-tanzu/cluster-api-provider-bringyourownhost/test/e2e"
	certv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
)

var _ = Describe("Agent", func() {

	Context("When the host is unable to register with the API server", func() {
		var (
			ns               *corev1.Namespace
			ctx              context.Context
			err              error
			hostName         string
			runner           *e2e.ByoHostRunner
			byoHostContainer *container.ContainerCreateCreatedBody
		)

		BeforeEach(func() {
			ns = builder.Namespace("testns").Build()
			ctx = context.TODO()
			Expect(k8sClient.Create(context.TODO(), ns)).NotTo(HaveOccurred(), "failed to create test namespace")

			hostName, err = os.Hostname()
			Expect(err).NotTo(HaveOccurred())
			runner = setupTestInfra(ctx, hostName, getKubeConfig().Name(), ns)

			byoHostContainer, err = runner.SetupByoDockerHost()
			Expect(err).NotTo(HaveOccurred())

		})

		AfterEach(func() {
			cleanup(runner.Context, byoHostContainer, ns, agentLogFile)
		})

		It("should not error out if the host already exists", func() {
			// not using the builder method here
			// because builder makes use of GenerateName that generates random names
			// For the below byoHost we need the name to be deterministic
			byoHost := &infrastructurev1beta1.ByoHost{
				TypeMeta: metav1.TypeMeta{
					Kind:       "ByoHost",
					APIVersion: "infrastructure.cluster.x-k8s.io/v1beta1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      hostName,
					Namespace: ns.Name,
				},
				Spec: infrastructurev1beta1.ByoHostSpec{},
			}
			Expect(k8sClient.Create(context.TODO(), byoHost)).NotTo(HaveOccurred())

			runner.CommandArgs["--downloadpath"] = fakeDownloadPath
			output, _, err := runner.ExecByoDockerHost(byoHostContainer)
			Expect(err).NotTo(HaveOccurred())

			defer output.Close()
			f := e2e.WriteDockerLog(output, agentLogFile)
			defer func() {
				deferredErr := f.Close()
				if deferredErr != nil {
					e2e.Showf("error closing file %s: %v", agentLogFile, deferredErr)
				}
			}()
			Consistently(func() (done bool) {
				_, err := os.Stat(agentLogFile)
				if err == nil {
					data, err := os.ReadFile(agentLogFile)
					if err == nil && strings.Contains(string(data), "\"msg\"=\"error\"") {
						return true
					}
				}
				return false
			}).Should(BeFalse())
		})

		It("should return an error when invalid kubeconfig is passed in", func() {

			runner.CommandArgs["--kubeconfig"] = fakeKubeConfig
			output, _, err := runner.ExecByoDockerHost(byoHostContainer)
			Expect(err).NotTo(HaveOccurred())
			defer output.Close()

			f := e2e.WriteDockerLog(output, agentLogFile)
			defer func() {
				deferredErr := f.Close()
				if deferredErr != nil {
					e2e.Showf("error closing file %s: %v", agentLogFile, deferredErr)
				}
			}()
			Eventually(func() (done bool) {
				_, err := os.Stat(agentLogFile)
				if err == nil {
					data, err := os.ReadFile(agentLogFile)
					if err == nil && strings.Contains(string(data), "\"msg\"=\"error getting kubeconfig\"") {
						return true
					}
				}
				return false
			}).Should(BeTrue())
		})
	})

	Context("When the host agent is able to connect to API Server", func() {

		var (
			ns               *corev1.Namespace
			ctx              context.Context
			hostName         string
			fakeDownloadPath = "fake-download-path"
			runner           *e2e.ByoHostRunner
			byoHostContainer *container.ContainerCreateCreatedBody
			output           dockertypes.HijackedResponse
		)

		BeforeEach(func() {
			ns = builder.Namespace("testns").Build()
			Expect(k8sClient.Create(context.TODO(), ns)).NotTo(HaveOccurred(), "failed to create test namespace")
			ctx = context.TODO()
			var err error
			hostName, err = os.Hostname()
			Expect(err).NotTo(HaveOccurred())

			runner = setupTestInfra(ctx, hostName, getKubeConfig().Name(), ns)
			runner.CommandArgs["--label"] = "site=apac"
			runner.CommandArgs["--downloadpath"] = fakeDownloadPath

			byoHostContainer, err = runner.SetupByoDockerHost()
			Expect(err).NotTo(HaveOccurred())

			output, _, err = runner.ExecByoDockerHost(byoHostContainer)
			Expect(err).NotTo(HaveOccurred())

			// wait until the agent process starts inside the byoh host container
			Eventually(func() bool {
				containerTop, _ := runner.DockerClient.ContainerTop(ctx, byoHostContainer.ID, []string{})
				for _, proc := range containerTop.Processes {
					if strings.Contains(proc[len(containerTop.Titles)-1], "agent") {
						return true
					}

				}
				return false
			}, 60).Should(BeTrue())
		})

		AfterEach(func() {
			cleanup(runner.Context, byoHostContainer, ns, agentLogFile)
		})

		It("should register the BYOHost with the management cluster", func() {
			byoHostLookupKey := types.NamespacedName{Name: hostName, Namespace: ns.Name}
			createdByoHost := &infrastructurev1beta1.ByoHost{}
			Eventually(func() *infrastructurev1beta1.ByoHost {
				err := k8sClient.Get(context.TODO(), byoHostLookupKey, createdByoHost)
				if err != nil {
					return nil
				}
				return createdByoHost
			}).ShouldNot(BeNil())
		})

		It("should register the BYOHost with the passed labels", func() {
			byoHostLookupKey := types.NamespacedName{Name: hostName, Namespace: ns.Name}
			createdByoHost := &infrastructurev1beta1.ByoHost{}
			Eventually(func() map[string]string {
				err := k8sClient.Get(context.TODO(), byoHostLookupKey, createdByoHost)
				if err != nil {
					return nil
				}
				return createdByoHost.ObjectMeta.Labels
			}).Should(Equal(map[string]string{"site": "apac"}))
		})

		It("should skip CSR creation in default mode", func() {
			defer output.Close()
			f := e2e.WriteDockerLog(output, agentLogFile)
			defer func() {
				deferredErr := f.Close()
				if deferredErr != nil {
					e2e.Showf("error closing file %s: %v", agentLogFile, deferredErr)
				}
			}()

			Consistently(func() (done bool) {
				_, err := os.Stat(agentLogFile)
				if err == nil {
					data, err := os.ReadFile(agentLogFile)
					if err == nil && !strings.Contains(string(data), "creating host csr") {
						return true
					}
				}
				return false
			}, time.Second*2).Should(BeTrue())
		})

		It("should fetch networkstatus when register the BYOHost with the management cluster", func() {
			byoHostLookupKey := types.NamespacedName{Name: hostName, Namespace: ns.Name}
			defaultIP, err := gateway.DiscoverInterface()
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() bool {
				createdByoHost := &infrastructurev1beta1.ByoHost{}
				err := k8sClient.Get(context.TODO(), byoHostLookupKey, createdByoHost)
				if err != nil {
					return false
				}
				// check if default ip and networkInterfaceName is right
				for _, item := range createdByoHost.Status.Network {
					if item.IsDefault {
						iface, err := net.InterfaceByName(item.NetworkInterfaceName)
						if err != nil {
							return false
						}

						addrs, err := iface.Addrs()
						if err != nil {
							return false
						}

						for _, addr := range addrs {
							var ip net.IP
							switch v := addr.(type) {
							case *net.IPNet:
								ip = v.IP
							case *net.IPAddr:
								ip = v.IP
							}
							if ip.String() == defaultIP.String() {
								return true
							}
						}
					}
				}
				return false
			}).Should(BeTrue())

		})

		It("should only reconcile ByoHost resource that the agent created", func() {
			byoHost := builder.ByoHost(ns.Name, "random-second-host").Build()
			Expect(k8sClient.Create(context.TODO(), byoHost)).NotTo(HaveOccurred(), "failed to create byohost")

			defer output.Close()

			f := e2e.WriteDockerLog(output, agentLogFile)
			defer func() {
				deferredErr := f.Close()
				if deferredErr != nil {
					e2e.Showf("error closing file %s: %v", agentLogFile, deferredErr)
				}
			}()
			Consistently(func() (done bool) {
				_, err := os.Stat(agentLogFile)
				if err == nil {
					data, err := os.ReadFile(agentLogFile)
					if err == nil && strings.Contains(string(data), byoHost.Name) {
						return true
					}
				}
				return false
			}, 10, 1).ShouldNot(BeTrue())
		})

		Context("when machineref & bootstrap secret is assigned", func() {
			var (
				byoMachine *infrastructurev1beta1.ByoMachine
				namespace  types.NamespacedName
			)
			BeforeEach(func() {
				byoMachine = builder.ByoMachine(ns.Name, defaultByoMachineName).Build()
				Expect(k8sClient.Create(ctx, byoMachine)).Should(Succeed())
				byoHost := &infrastructurev1beta1.ByoHost{}
				namespace = types.NamespacedName{Name: hostName, Namespace: ns.Name}
				Eventually(func() (err error) {
					err = k8sClient.Get(ctx, namespace, byoHost)
					return err
				}).Should(BeNil())

				patchHelper, _ := patch.NewHelper(byoHost, k8sClient)
				byoHost.Status.MachineRef = &corev1.ObjectReference{
					APIVersion: byoMachine.APIVersion,
					Kind:       byoMachine.Kind,
					Namespace:  byoMachine.Namespace,
					Name:       byoMachine.Name,
					UID:        byoMachine.UID,
				}
				byoHost.Annotations = map[string]string{}
				byoHost.Annotations[infrastructurev1beta1.K8sVersionAnnotation] = K8sVersion
				byoHost.Annotations[infrastructurev1beta1.BundleLookupBaseRegistryAnnotation] = bundleLookupBaseRegistry
				byoHost.Annotations[infrastructurev1beta1.BundleLookupTagAnnotation] = BundleLookupTag

				fakeBootstrapSecret := builder.Secret(ns.Name, fakeBootstrapSecret).Build()
				err := k8sClient.Create(ctx, fakeBootstrapSecret)
				Expect(err).ToNot(HaveOccurred())
				byoHost.Spec.BootstrapSecret = &corev1.ObjectReference{
					Kind:      "Secret",
					Namespace: byoMachine.Namespace,
					Name:      fakeBootstrapSecret.Name,
				}

				Expect(patchHelper.Patch(ctx, byoHost, patch.WithStatusObservedGeneration{})).NotTo(HaveOccurred())
			})

			It("should install k8s components", func() {

				defer output.Close()
				f := e2e.WriteDockerLog(output, agentLogFile)
				defer func() {
					deferredErr := f.Close()
					if deferredErr != nil {
						e2e.Showf("error closing file %s: %v", agentLogFile, deferredErr)
					}
				}()
				updatedByoHost := &infrastructurev1beta1.ByoHost{}
				Eventually(func() (condition corev1.ConditionStatus) {
					err := k8sClient.Get(ctx, namespace, updatedByoHost)
					if err == nil {
						kubeInstallStatus := conditions.Get(updatedByoHost, infrastructurev1beta1.K8sComponentsInstallationSucceeded)
						if kubeInstallStatus != nil {
							return kubeInstallStatus.Status
						}
					}
					return corev1.ConditionFalse
				}, 100).Should(Equal(corev1.ConditionTrue)) // installing K8s components is a lengthy operation, setting the timeout to 100s
			})
		})
	})

	Context("When host agent is executed with --version flag", func() {
		var (
			tmpHostAgentBinary string
		)
		BeforeEach(func() {
			date, err := exec.Command("date").Output()
			Expect(err).NotTo(HaveOccurred())

			version.GitMajor = "1"
			version.GitMinor = "2"
			version.GitVersion = "v1.2.3"
			version.GitCommit = "abc"
			version.GitTreeState = "clean"
			version.BuildDate = string(date)

			ldflags := fmt.Sprintf("-X 'github.com/vmware-tanzu/cluster-api-provider-bringyourownhost/agent/version.GitMajor=%s'"+
				"-X 'github.com/vmware-tanzu/cluster-api-provider-bringyourownhost/agent/version.GitMinor=%s'"+
				"-X 'github.com/vmware-tanzu/cluster-api-provider-bringyourownhost/agent/version.GitVersion=%s'"+
				"-X 'github.com/vmware-tanzu/cluster-api-provider-bringyourownhost/agent/version.GitCommit=%s'"+
				"-X 'github.com/vmware-tanzu/cluster-api-provider-bringyourownhost/agent/version.GitTreeState=%s'"+
				"-X 'github.com/vmware-tanzu/cluster-api-provider-bringyourownhost/agent/version.BuildDate=%s'",
				version.GitMajor, version.GitMinor, version.GitVersion, version.GitCommit, version.GitTreeState, version.BuildDate)

			tmpHostAgentBinary, err = gexec.Build("github.com/vmware-tanzu/cluster-api-provider-bringyourownhost/agent", "-ldflags", ldflags)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			version.GitMajor = ""
			version.GitMinor = ""
			version.GitVersion = ""
			version.GitCommit = ""
			version.GitTreeState = ""
			version.BuildDate = ""
			tmpHostAgentBinary = ""
		})

		It("Shows the appropriate version of the agent", func() {
			expectedStruct := version.Info{
				Major:        "1",
				Minor:        "2",
				GitVersion:   "v1.2.3",
				GitCommit:    "abc",
				GitTreeState: "clean",
				BuildDate:    version.BuildDate,
				GoVersion:    runtime.Version(),
				Compiler:     runtime.Compiler,
				Platform:     fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
			}
			expected := fmt.Sprintf("byoh-hostagent version: %#v\n", expectedStruct)
			out, err := exec.Command(tmpHostAgentBinary, "--version").Output()
			Expect(err).NotTo(HaveOccurred())
			output := string(out)
			Expect(output).Should(Equal(expected))
		})
	})

	Context("When --version flag is created using 'version.sh' script", func() {
		var (
			tmpHostAgentBinary string
			gitMajor           string
			gitMinor           string
			gitVersion         string
			err                error
		)
		BeforeEach(func() {
			command := exec.Command("/bin/sh", "-c", "git describe --tags --abbrev=14 --match 'v[0-9]*' 2>/dev/null")
			command.Stderr = os.Stderr
			cmdOut, _ := command.Output()
			gitVersion = strings.TrimSuffix(string(cmdOut), "\n")

			gitVersion = strings.Split(gitVersion, "-")[0]
			gitVars := strings.Split(gitVersion, ".")
			if len(gitVars) > 1 {
				gitMajor = gitVars[0][1:]
				gitMinor = gitVars[1]
			}

			root, _ := exec.Command("/bin/sh", "-c", "git rev-parse --show-toplevel").Output()
			cmd := exec.Command("/bin/sh", "-c", strings.TrimSuffix(string(root), "\n")+"/hack/version.sh")
			ldflags, _ := cmd.Output()
			tmpHostAgentBinary, err = gexec.Build("github.com/vmware-tanzu/cluster-api-provider-bringyourownhost/agent", "-ldflags", string(ldflags))
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			tmpHostAgentBinary = ""
			gitMajor = ""
			gitMinor = ""
			gitVersion = ""
		})

		It("should match local generated git values", func() {
			out, err := exec.Command(tmpHostAgentBinary, "--version").Output()
			Expect(err).NotTo(HaveOccurred())

			majorExpected := "Major:\"" + gitMajor + "\""
			Expect(out).Should(ContainSubstring(majorExpected))

			minorExpected := "Minor:\"" + gitMinor + "\""
			Expect(out).Should(ContainSubstring(minorExpected))

			gitVersionExpected := "GitVersion:\"" + gitVersion
			Expect(out).Should(ContainSubstring(gitVersionExpected))

		})
	})

	Context("When the host agent is executed with --skip-installation flag", func() {
		var (
			ns               *corev1.Namespace
			ctx              context.Context
			err              error
			hostName         string
			fakeDownloadPath = "fake-download-path"
			runner           *e2e.ByoHostRunner
			byoHostContainer *container.ContainerCreateCreatedBody
		)

		BeforeEach(func() {
			ns = builder.Namespace("testns").Build()
			ctx = context.TODO()
			Expect(k8sClient.Create(context.TODO(), ns)).NotTo(HaveOccurred(), "failed to create test namespace")

			hostName, err = os.Hostname()
			Expect(err).NotTo(HaveOccurred())
			runner = setupTestInfra(ctx, hostName, getKubeConfig().Name(), ns)

			byoHostContainer, err = runner.SetupByoDockerHost()
			Expect(err).NotTo(HaveOccurred())

		})

		AfterEach(func() {
			cleanup(runner.Context, byoHostContainer, ns, agentLogFile)
		})

		It("should skip installation of k8s components", func() {
			runner.CommandArgs["--downloadpath"] = fakeDownloadPath
			runner.CommandArgs["--skip-installation"] = ""
			output, _, err := runner.ExecByoDockerHost(byoHostContainer)
			Expect(err).NotTo(HaveOccurred())

			defer output.Close()
			f := e2e.WriteDockerLog(output, agentLogFile)
			defer func() {
				deferredErr := f.Close()
				if deferredErr != nil {
					e2e.Showf("error closing file %s: %v", agentLogFile, deferredErr)
				}
			}()
			Eventually(func() (done bool) {
				_, err := os.Stat(agentLogFile)
				if err == nil {
					data, err := os.ReadFile(agentLogFile)
					if err == nil && strings.Contains(string(data), "\"msg\"=\"skip-installation flag set, skipping installer initialisation\"") {
						return true
					}
				}
				return false
			}, 30).Should(BeTrue())
		})
	})

	Context("When the host agent is executed with SecureAccess feature flag", func() {

		var (
			ns               *corev1.Namespace
			ctx              context.Context
			hostName         string
			runner           *e2e.ByoHostRunner
			byoHostContainer *container.ContainerCreateCreatedBody
			output           dockertypes.HijackedResponse
		)

		BeforeEach(func() {
			ns = builder.Namespace("testns").Build()
			ctx = context.TODO()
			Expect(k8sClient.Create(ctx, ns)).NotTo(HaveOccurred(), "failed to create test namespace")

			var err error
			hostName, err = os.Hostname()
			Expect(err).NotTo(HaveOccurred())

			runner = setupTestInfra(ctx, hostName, getKubeConfig().Name(), ns)
			runner.CommandArgs["--feature-gates"] = "SecureAccess=true"
			runner.CommandArgs["--bootstrap-kubeconfig"] = "/mgmt.conf"
			byoHostContainer, err = runner.SetupByoDockerHost()
			Expect(err).NotTo(HaveOccurred())

			output, _, err = runner.ExecByoDockerHost(byoHostContainer)
			Expect(err).NotTo(HaveOccurred())
			// Clean for any CSR present
			var csrList certv1.CertificateSigningRequestList
			Expect(k8sClient.List(ctx, &csrList)).ShouldNot(HaveOccurred())
			for _, csr := range csrList.Items {
				Expect(k8sClient.Delete(ctx, &csr)).ShouldNot(HaveOccurred())
			}
		})

		JustAfterEach(func() {
			if CurrentGinkgoTestDescription().Failed {
				e2e.ShowFileContent(agentLogFile)
			}
		})

		AfterEach(func() {
			cleanup(runner.Context, byoHostContainer, ns, agentLogFile)
		})

		It("should enable the SecureAccess feature gate", func() {
			defer output.Close()
			f := e2e.WriteDockerLog(output, agentLogFile)
			defer func() {
				deferredErr := f.Close()
				if deferredErr != nil {
					e2e.Showf("error closing file %s: %v", agentLogFile, deferredErr)
				}
			}()
			Eventually(func() (done bool) {
				_, err := os.Stat(agentLogFile)
				if err == nil {
					data, err := os.ReadFile(agentLogFile)
					if err == nil && strings.Contains(string(data), "\"msg\"=\"secure access enabled, waiting for host to be registered by ByoAdmission Controller\"") {
						return true
					}
				}
				return false
			}, time.Second*2).Should(BeTrue())
		})
		It("should not register the BYOHost with the management cluster", func() {
			byoHostLookupKey := types.NamespacedName{Name: hostName, Namespace: ns.Name}
			createdByoHost := &infrastructurev1beta1.ByoHost{}
			Consistently(func() *infrastructurev1beta1.ByoHost {
				err := k8sClient.Get(context.TODO(), byoHostLookupKey, createdByoHost)
				if err != nil {
					return nil
				}
				return createdByoHost
			}).Should(BeNil())
		})
		It("should create BYOHost CSR in the management cluster", func() {
			defer output.Close()
			f := e2e.WriteDockerLog(output, agentLogFile)
			defer func() {
				deferredErr := f.Close()
				if deferredErr != nil {
					e2e.Showf("error closing file %s: %v", agentLogFile, deferredErr)
				}
			}()
			byohCSRLookupKey := types.NamespacedName{Name: fmt.Sprintf(registration.ByohCSRNameFormat, hostName)}
			byohCSR := &certv1.CertificateSigningRequest{}

			Eventually(func() string {
				err := k8sClient.Get(context.TODO(), byohCSRLookupKey, byohCSR)
				if err != nil {
					return err.Error()
				}
				return byohCSR.Name
			}, 10, 1).Should(Equal(fmt.Sprintf(registration.ByohCSRNameFormat, hostName)))
		})
		It("should persist private key", func() {
			defer output.Close()
			fAgent := e2e.WriteDockerLog(output, agentLogFile)
			defer func() {
				deferredErr := fAgent.Close()
				if deferredErr != nil {
					e2e.Showf("error closing file %s: %v", agentLogFile, deferredErr)
				}
			}()
			// exec in container to check the file
			cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
			Expect(err).ShouldNot(HaveOccurred())
			time.Sleep(4 * time.Second)
			response, err := cli.ContainerExecCreate(ctx, byoHostContainer.ID, dockertypes.ExecConfig{
				AttachStdin:  false,
				AttachStdout: true,
				AttachStderr: true,
				Cmd:          []string{"cat", registration.TmpPrivateKey},
			})
			Expect(err).ShouldNot(HaveOccurred())
			result, err := cli.ContainerExecAttach(ctx, response.ID, dockertypes.ExecStartCheck{})
			Expect(err).ShouldNot(HaveOccurred())
			defer result.Close()
			fExec := e2e.WriteDockerLog(result, execLogFile)
			defer func() {
				deferredErr := fExec.Close()
				if deferredErr != nil {
					e2e.Showf("error closing file %s: %v", execLogFile, deferredErr)
				}
			}()
			Eventually(func() (done bool) {
				_, err := os.Stat(execLogFile)
				if err == nil {
					data, err := os.ReadFile(execLogFile)
					if err == nil && strings.Contains(string(data), "PRIVATE KEY") {
						return true
					}
				}
				return false
			}).Should(BeTrue())
			Expect(os.Remove(execLogFile)).ShouldNot(HaveOccurred())
		})
		It("should wait for the certificate to be issued", func() {
			defer output.Close()
			f := e2e.WriteDockerLog(output, agentLogFile)
			defer func() {
				deferredErr := f.Close()
				if deferredErr != nil {
					e2e.Showf("error closing file %s: %v", agentLogFile, deferredErr)
				}
			}()
			Eventually(func() (done bool) {
				_, err := os.Stat(agentLogFile)
				if err == nil {
					data, err := os.ReadFile(agentLogFile)
					if err == nil && strings.Contains(string(data), "\"msg\"=\"Waiting for client certificate to be issued\"") {
						return true
					}
				}
				return false
			}, time.Second*4).Should(BeTrue())
		})
		It("should create kubeconfig if the csr is approved", func() {
			defer output.Close()
			fAgent := e2e.WriteDockerLog(output, agentLogFile)
			defer func() {
				deferredErr := fAgent.Close()
				if deferredErr != nil {
					e2e.Showf("error closing file %s: %v", agentLogFile, deferredErr)
				}
			}()

			// Approve CSR
			Eventually(func() (done bool) {
				byohCSR, err := clientSet.CertificatesV1().CertificateSigningRequests().Get(ctx, fmt.Sprintf(registration.ByohCSRNameFormat, hostName), metav1.GetOptions{})
				if err != nil {
					return false
				}
				byohCSR.Status.Conditions = append(byohCSR.Status.Conditions, certv1.CertificateSigningRequestCondition{
					Type:    certv1.CertificateApproved,
					Reason:  "approved",
					Message: "approved",
					Status:  corev1.ConditionTrue,
				})
				_, err = clientSet.CertificatesV1().CertificateSigningRequests().UpdateApproval(ctx, fmt.Sprintf(registration.ByohCSRNameFormat, hostName), byohCSR, metav1.UpdateOptions{})
				return err == nil
			}, time.Second*4).Should(BeTrue())
			// Issue Certificate
			byohCSR, err := clientSet.CertificatesV1().CertificateSigningRequests().Get(ctx, fmt.Sprintf(registration.ByohCSRNameFormat, hostName), metav1.GetOptions{})
			Expect(err).ShouldNot(HaveOccurred())
			var FakeCert = `
-----BEGIN CERTIFICATE-----
MIIBvzCCAWWgAwIBAgIRAMd7Mz3fPrLm1aFUn02lLHowCgYIKoZIzj0EAwIwIzEh
MB8GA1UEAwwYazNzLWNsaWVudC1jYUAxNjE2NDMxOTU2MB4XDTIxMDQxOTIxNTMz
MFoXDTIyMDQxOTIxNTMzMFowMjEVMBMGA1UEChMMc3lzdGVtOm5vZGVzMRkwFwYD
VQQDExBzeXN0ZW06bm9kZTp0ZXN0MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE
Xd9aZm6nftepZpUwof9RSUZqZDgu7dplIiDt8nnhO5Bquy2jn7/AVx20xb0Xz0d2
XLn3nn5M+lR2p3NlZmqWHaNrMGkwDgYDVR0PAQH/BAQDAgWgMBMGA1UdJQQMMAoG
CCsGAQUFBwMBMAwGA1UdEwEB/wQCMAAwHwYDVR0jBBgwFoAU/fZa5enijRDB25DF
NT1/vPUy/hMwEwYDVR0RBAwwCoIIRE5TOnRlc3QwCgYIKoZIzj0EAwIDSAAwRQIg
b3JL5+Q3zgwFrciwfdgtrKv8MudlA0nu6EDQO7eaJbwCIQDegFyC4tjGPp/5JKqQ
kovW9X7Ook/tTW0HyX6D6HRciA==
-----END CERTIFICATE-----
`
			byohCSR.Status.Certificate = []byte(FakeCert)
			_, err = clientSet.CertificatesV1().CertificateSigningRequests().UpdateStatus(ctx, byohCSR, metav1.UpdateOptions{})
			Expect(err).ShouldNot(HaveOccurred())

			cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
			Expect(err).ShouldNot(HaveOccurred())
			time.Sleep(2 * time.Second)
			response, err := cli.ContainerExecCreate(ctx, byoHostContainer.ID, dockertypes.ExecConfig{
				AttachStdin:  false,
				AttachStdout: true,
				AttachStderr: true,
				Cmd:          []string{"cat", "~/.byoh/config"},
			})
			Expect(err).ShouldNot(HaveOccurred())
			result, err := cli.ContainerExecAttach(ctx, response.ID, dockertypes.ExecStartCheck{})
			Expect(err).ShouldNot(HaveOccurred())
			defer result.Close()
			fExec := e2e.WriteDockerLog(result, execLogFile)
			defer func() {
				deferredErr := fExec.Close()
				if deferredErr != nil {
					e2e.Showf("error closing file %s: %v", execLogFile, deferredErr)
				}
			}()
			Eventually(func() (done bool) {
				_, err := os.Stat(execLogFile)
				if err == nil {
					data, err := os.ReadFile(execLogFile)
					if err == nil && strings.Contains(string(data), "name: default-cluster") {
						return true
					}
				}
				return false
			}, time.Second*4).Should(BeTrue())
			Expect(os.Remove(execLogFile)).ShouldNot(HaveOccurred())
		})
	})

	Context("When the host agent is executed with --use-installer-controller flag", func() {
		var (
			ns               *corev1.Namespace
			ctx              context.Context
			err              error
			hostName         string
			runner           *e2e.ByoHostRunner
			byoHostContainer *container.ContainerCreateCreatedBody
			output           dockertypes.HijackedResponse
		)

		BeforeEach(func() {
			ns = builder.Namespace("testns").Build()
			ctx = context.TODO()
			Expect(k8sClient.Create(ctx, ns)).NotTo(HaveOccurred(), "failed to create test namespace")

			hostName, err = os.Hostname()
			Expect(err).NotTo(HaveOccurred())

			runner = setupTestInfra(ctx, hostName, getKubeConfig().Name(), ns)
			runner.CommandArgs["--use-installer-controller"] = ""

			byoHostContainer, err = runner.SetupByoDockerHost()
			Expect(err).NotTo(HaveOccurred())

		})

		AfterEach(func() {
			cleanup(runner.Context, byoHostContainer, ns, agentLogFile)
		})

		It("should not call the intree installer", func() {
			output, _, err = runner.ExecByoDockerHost(byoHostContainer)
			Expect(err).NotTo(HaveOccurred())
			defer output.Close()
			f := e2e.WriteDockerLog(output, agentLogFile)
			defer func() {
				deferredErr := f.Close()
				if deferredErr != nil {
					e2e.Showf("error closing file %s: %v", agentLogFile, deferredErr)
				}
			}()
			Eventually(func() (done bool) {
				_, err := os.Stat(agentLogFile)
				if err == nil {
					data, err := os.ReadFile(agentLogFile)
					if err == nil && strings.Contains(string(data), "\"msg\"=\"use-installer-controller flag set, skipping intree installer\"") {
						return true
					}
				}
				return false
			}, 30).Should(BeTrue())
		})
	})
})
