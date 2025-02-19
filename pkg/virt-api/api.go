/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2018 Red Hat, Inc.
 *
 */

package virt_api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	restful "github.com/emicklei/go-restful"
	"github.com/go-openapi/spec"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	flag "github.com/spf13/pflag"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
	certificate2 "k8s.io/client-go/util/certificate"
	aggregatorclient "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"

	v1 "kubevirt.io/client-go/api/v1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/log"
	clientutil "kubevirt.io/client-go/util"
	virtversion "kubevirt.io/client-go/version"
	"kubevirt.io/kubevirt/pkg/certificates/bootstrap"
	"kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/healthz"
	"kubevirt.io/kubevirt/pkg/rest/filter"
	"kubevirt.io/kubevirt/pkg/service"
	"kubevirt.io/kubevirt/pkg/util"
	"kubevirt.io/kubevirt/pkg/util/openapi"
	webhooksutils "kubevirt.io/kubevirt/pkg/util/webhooks"
	"kubevirt.io/kubevirt/pkg/virt-api/rest"
	"kubevirt.io/kubevirt/pkg/virt-api/webhooks"
	mutating_webhook "kubevirt.io/kubevirt/pkg/virt-api/webhooks/mutating-webhook"
	validating_webhook "kubevirt.io/kubevirt/pkg/virt-api/webhooks/validating-webhook"
	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
	"kubevirt.io/kubevirt/pkg/virt-operator/resource/generate/components"
	virtoperatorutils "kubevirt.io/kubevirt/pkg/virt-operator/util"
)

const (
	// Default port that virt-api listens on.
	defaultPort = 443

	// Default address that virt-api listens on.
	defaultHost = "0.0.0.0"

	defaultConsoleServerPort = 8186

	defaultCAConfigMapName     = "kubevirt-ca"
	defaultTlsCertFilePath     = "/etc/virt-api/certificates/tls.crt"
	defaultTlsKeyFilePath      = "/etc/virt-api/certificates/tls.key"
	defaultHandlerCertFilePath = "/etc/virt-handler/clientcertificates/tls.crt"
	defaultHandlerKeyFilePath  = "/etc/virt-handler/clientcertificates/tls.key"

	httpStatusNotFoundMessage     = "Not Found"
	httpStatusBadRequestMessage   = "Bad Request"
	httpStatusInternalServerError = "Internal Server Error"
)

type VirtApi interface {
	Compose()
	Run()
	AddFlags()
	ConfigureOpenAPIService()
	Execute()
}

type virtAPIApp struct {
	service.ServiceListen
	SwaggerUI        string
	SubresourcesOnly bool
	virtCli          kubecli.KubevirtClient
	aggregatorClient *aggregatorclient.Clientset
	authorizor       rest.VirtApiAuthorizor
	certsDirectory   string
	clusterConfig    *virtconfig.ClusterConfig

	namespace               string
	host                    string
	tlsConfig               *tls.Config
	certificate             *tls.Certificate
	consoleServerPort       int
	certmanager             certificate2.Manager
	handlerTLSConfiguration *tls.Config
	handlerCertManager      certificate2.Manager

	caConfigMapName     string
	tlsCertFilePath     string
	tlsKeyFilePath      string
	handlerCertFilePath string
	handlerKeyFilePath  string
	externallyManaged   bool
}

var (
	_                service.Service = &virtAPIApp{}
	apiHealthVersion                 = new(healthz.KubeApiHealthzVersion)
)

func NewVirtApi() VirtApi {

	app := &virtAPIApp{}
	app.BindAddress = defaultHost
	app.Port = defaultPort

	return app
}

func (app *virtAPIApp) Execute() {
	virtCli, err := kubecli.GetKubevirtClient()
	if err != nil {
		panic(err)
	}

	authorizor, err := rest.NewAuthorizor()
	if err != nil {
		panic(err)
	}

	config, err := kubecli.GetConfig()
	if err != nil {
		panic(err)
	}

	app.aggregatorClient = aggregatorclient.NewForConfigOrDie(config)

	app.authorizor = authorizor

	app.virtCli = virtCli

	app.certsDirectory, err = ioutil.TempDir("", "certsdir")
	if err != nil {
		panic(err)
	}
	app.namespace, err = clientutil.GetNamespace()
	if err != nil {
		panic(err)
	}

	app.ConfigureOpenAPIService()
	app.Run()
}

func subresourceAPIGroup() metav1.APIGroup {
	apiGroup := metav1.APIGroup{
		Name: "subresource.kubevirt.io",
		PreferredVersion: metav1.GroupVersionForDiscovery{
			GroupVersion: v1.SubresourceGroupVersions[0].Group + "/" + v1.SubresourceGroupVersions[0].Version,
			Version:      v1.SubresourceGroupVersions[0].Version,
		},
	}

	for _, version := range v1.SubresourceGroupVersions {
		apiGroup.Versions = append(apiGroup.Versions, metav1.GroupVersionForDiscovery{
			GroupVersion: version.Group + "/" + version.Version,
			Version:      version.Version,
		})
	}
	apiGroup.ServerAddressByClientCIDRs = append(apiGroup.ServerAddressByClientCIDRs, metav1.ServerAddressByClientCIDR{
		ClientCIDR:    "0.0.0.0/0",
		ServerAddress: "",
	})
	apiGroup.Kind = "APIGroup"
	return apiGroup
}

func (app *virtAPIApp) composeSubresources() {

	var subwss []*restful.WebService

	for _, version := range v1.SubresourceGroupVersions {
		subresourcesvmGVR := schema.GroupVersionResource{Group: version.Group, Version: version.Version, Resource: "virtualmachines"}
		subresourcesvmiGVR := schema.GroupVersionResource{Group: version.Group, Version: version.Version, Resource: "virtualmachineinstances"}

		subws := new(restful.WebService)
		subws.Doc(fmt.Sprintf("KubeVirt \"%s\" Subresource API.", version.Version))
		subws.Path(rest.GroupVersionBasePath(version))

		subresourceApp := rest.NewSubresourceAPIApp(app.virtCli, app.consoleServerPort, app.handlerTLSConfiguration, app.clusterConfig)

		restartRouteBuilder := subws.PUT(rest.ResourcePath(subresourcesvmGVR)+rest.SubResourcePath("restart")).
			To(subresourceApp.RestartVMRequestHandler).
			Reads(v1.RestartOptions{}).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation(version.Version+"Restart").
			Doc("Restart a VirtualMachine object.").
			Returns(http.StatusOK, "OK", "").
			Returns(http.StatusNotFound, httpStatusNotFoundMessage, "").
			Returns(http.StatusBadRequest, httpStatusBadRequestMessage, "")
		restartRouteBuilder.ParameterNamed("body").Required(false)
		subws.Route(restartRouteBuilder)

		subws.Route(subws.PUT(rest.ResourcePath(subresourcesvmGVR)+rest.SubResourcePath("migrate")).
			To(subresourceApp.MigrateVMRequestHandler).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation(version.Version+"Migrate").
			Doc("Migrate a running VirtualMachine to another node.").
			Returns(http.StatusOK, "OK", "").
			Returns(http.StatusNotFound, httpStatusNotFoundMessage, "").
			Returns(http.StatusBadRequest, httpStatusBadRequestMessage, ""))

		subws.Route(subws.PUT(rest.ResourcePath(subresourcesvmGVR)+rest.SubResourcePath("cancel_migrate")).
			To(subresourceApp.MigrateCancelVMRequestHandler).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation(version.Version+"MigrateCancel").
			Doc("Cancel migration of VM.").
			Returns(http.StatusOK, "OK", "").
			Returns(http.StatusNotFound, httpStatusNotFoundMessage, "").
			Returns(http.StatusBadRequest, httpStatusBadRequestMessage, ""))

		subws.Route(subws.PUT(rest.ResourcePath(subresourcesvmGVR)+rest.SubResourcePath("start")).
			To(subresourceApp.StartVMRequestHandler).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation(version.Version+"Start").
			Doc("Start a VirtualMachine object.").
			Returns(http.StatusOK, "OK", "").
			Returns(http.StatusNotFound, httpStatusNotFoundMessage, "").
			Returns(http.StatusBadRequest, httpStatusBadRequestMessage, ""))

		stopRouteBuilder := subws.PUT(rest.ResourcePath(subresourcesvmGVR)+rest.SubResourcePath("stop")).
			To(subresourceApp.StopVMRequestHandler).
			Reads(v1.StopOptions{}).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation(version.Version+"Stop").
			Doc("Stop a VirtualMachine object.").
			Returns(http.StatusOK, "OK", "").
			Returns(http.StatusNotFound, httpStatusNotFoundMessage, "").
			Returns(http.StatusBadRequest, httpStatusBadRequestMessage, "")
		stopRouteBuilder.ParameterNamed("body").Required(false)
		subws.Route(stopRouteBuilder)

		subws.Route(subws.PUT(rest.ResourcePath(subresourcesvmiGVR)+rest.SubResourcePath("freeze")).
			To(subresourceApp.FreezeVMIRequestHandler).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation(version.Version+"Freeze").
			Doc("Freeze a VirtualMachineInstance object.").
			Returns(http.StatusOK, "OK", "").
			Returns(http.StatusInternalServerError, httpStatusInternalServerError, ""))

		subws.Route(subws.PUT(rest.ResourcePath(subresourcesvmiGVR)+rest.SubResourcePath("unfreeze")).
			To(subresourceApp.UnfreezeVMIRequestHandler).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation(version.Version+"Unfreeze").
			Doc("Unfreeze a VirtualMachineInstance object.").
			Returns(http.StatusOK, "OK", "").
			Returns(http.StatusInternalServerError, httpStatusInternalServerError, ""))

		subws.Route(subws.PUT(rest.ResourcePath(subresourcesvmiGVR)+rest.SubResourcePath("pause")).
			To(subresourceApp.PauseVMIRequestHandler).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation(version.Version+"Pause").
			Doc("Pause a VirtualMachineInstance object.").
			Returns(http.StatusOK, "OK", "").
			Returns(http.StatusNotFound, httpStatusNotFoundMessage, "").
			Returns(http.StatusBadRequest, httpStatusBadRequestMessage, ""))

		subws.Route(subws.PUT(rest.ResourcePath(subresourcesvmiGVR)+rest.SubResourcePath("unpause")).
			To(subresourceApp.UnpauseVMIRequestHandler). // handles VMIs as well
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation(version.Version+"Unpause").
			Doc("Unpause a VirtualMachineInstance object.").
			Returns(http.StatusOK, "OK", "").
			Returns(http.StatusNotFound, httpStatusNotFoundMessage, "").
			Returns(http.StatusBadRequest, httpStatusBadRequestMessage, ""))

		subws.Route(subws.GET(rest.ResourcePath(subresourcesvmiGVR) + rest.SubResourcePath("console")).
			To(subresourceApp.ConsoleRequestHandler).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation(version.Version + "Console").
			Doc("Open a websocket connection to a serial console on the specified VirtualMachineInstance."))

		subws.Route(subws.GET(rest.ResourcePath(subresourcesvmiGVR) + rest.SubResourcePath("vnc")).
			To(subresourceApp.VNCRequestHandler).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation(version.Version + "VNC").
			Doc("Open a websocket connection to connect to VNC on the specified VirtualMachineInstance."))

		subws.Route(subws.GET(rest.ResourcePath(subresourcesvmiGVR) + rest.SubResourcePath("usbredir")).
			To(subresourceApp.USBRedirRequestHandler).
			Param(rest.NamespaceParam(subws)).
			Param(rest.NameParam(subws)).
			Operation(version.Version + "usbredir").
			Doc("Open a websocket connection to connect to USB device on the specified VirtualMachineInstance."))

		// VMI endpoint
		subws.Route(subws.GET(rest.ResourcePath(subresourcesvmiGVR) + rest.SubResourcePath("portforward") + rest.PortPath).
			To(subresourceApp.PortForwardRequestHandler(subresourceApp.FetchVirtualMachineInstance)).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Param(rest.PortForwardPortParameter(subws)).
			Operation(version.Version + "vmi-PortForward").
			Doc("Open a websocket connection forwarding traffic to the specified VirtualMachineInstance and port."))
		subws.Route(subws.GET(rest.ResourcePath(subresourcesvmiGVR) + rest.SubResourcePath("portforward") + rest.PortPath + rest.ProtocolPath).
			To(subresourceApp.PortForwardRequestHandler(subresourceApp.FetchVirtualMachineInstance)).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Param(rest.PortForwardPortParameter(subws)).
			Param(rest.PortForwardProtocolParameter(subws)).
			Operation(version.Version + "vmi-PortForwardWithProtocol").
			Doc("Open a websocket connection forwarding traffic of the specified protocol (either tcp or udp) to the specified VirtualMachineInstance and port."))

		// VM endpoint
		subws.Route(subws.GET(rest.ResourcePath(subresourcesvmGVR) + rest.SubResourcePath("portforward") + rest.PortPath).
			To(subresourceApp.PortForwardRequestHandler(subresourceApp.FetchVirtualMachineInstanceForVM)).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Param(rest.PortForwardPortParameter(subws)).
			Operation(version.Version + "vm-PortForward").
			Doc("Open a websocket connection forwarding traffic to the running VMI for the specified VirtualMachine and port."))
		subws.Route(subws.GET(rest.ResourcePath(subresourcesvmGVR) + rest.SubResourcePath("portforward") + rest.PortPath + rest.ProtocolPath).
			To(subresourceApp.PortForwardRequestHandler(subresourceApp.FetchVirtualMachineInstanceForVM)).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Param(rest.PortForwardPortParameter(subws)).
			Param(rest.PortForwardProtocolParameter(subws)).
			Operation(version.Version + "vm-PortForwardWithProtocol").
			Doc("Open a websocket connection forwarding traffic of the specified protocol (either tcp or udp) to the specified VirtualMachine and port."))

		// An empty handler function would respond with HTTP OK by default
		subws.Route(subws.GET(rest.ResourcePath(subresourcesvmiGVR) + rest.SubResourcePath("test")).
			To(func(request *restful.Request, response *restful.Response) {}).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation(version.Version + "Test").
			Doc("Test endpoint verifying apiserver connectivity."))

		subws.Route(subws.GET(rest.SubResourcePath("version")).Produces(restful.MIME_JSON).
			To(func(request *restful.Request, response *restful.Response) {
				response.WriteAsJson(virtversion.Get())
			}).Operation(version.Version + "Version"))
		subws.Route(subws.GET(rest.SubResourcePath("guestfs")).Produces(restful.MIME_JSON).
			To(app.GetGsInfo()).
			Operation(version.Version+"Guestfs").
			Returns(http.StatusOK, "OK", "").
			Returns(http.StatusBadRequest, httpStatusBadRequestMessage, ""))
		subws.Route(subws.GET(rest.SubResourcePath("healthz")).
			To(healthz.KubeConnectionHealthzFuncFactory(app.clusterConfig, apiHealthVersion)).
			Consumes(restful.MIME_JSON).
			Produces(restful.MIME_JSON).
			Operation(version.Version+"CheckHealth").
			Doc("Health endpoint").
			Returns(http.StatusOK, "OK", "").
			Returns(http.StatusInternalServerError, "Unhealthy", ""))
		subws.Route(subws.GET(rest.ResourcePath(subresourcesvmiGVR)+rest.SubResourcePath("guestosinfo")).
			To(subresourceApp.GuestOSInfo).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Consumes(restful.MIME_JSON).
			Produces(restful.MIME_JSON).
			Operation(version.Version+"Guestosinfo").
			Doc("Get guest agent os information").
			Writes(v1.VirtualMachineInstanceGuestAgentInfo{}).
			Returns(http.StatusOK, "OK", v1.VirtualMachineInstanceGuestAgentInfo{}))

		subws.Route(subws.GET(rest.ResourcePath(subresourcesvmiGVR)+rest.SubResourcePath("userlist")).
			To(subresourceApp.UserList).
			Consumes(restful.MIME_JSON).
			Produces(restful.MIME_JSON).
			Operation(version.Version+"Userlist").
			Doc("Get list of active users via guest agent").
			Writes(v1.VirtualMachineInstanceGuestOSUserList{}).
			Returns(http.StatusOK, "OK", v1.VirtualMachineInstanceGuestOSUserList{}))

		subws.Route(subws.GET(rest.ResourcePath(subresourcesvmiGVR)+rest.SubResourcePath("filesystemlist")).
			To(subresourceApp.FilesystemList).
			Consumes(restful.MIME_JSON).
			Produces(restful.MIME_JSON).
			Operation(version.Version+"Filesystemlist").
			Doc("Get list of active filesystems on guest machine via guest agent").
			Writes(v1.VirtualMachineInstanceFileSystemList{}).
			Returns(http.StatusOK, "OK", v1.VirtualMachineInstanceFileSystemList{}))

		subws.Route(subws.PUT(rest.ResourcePath(subresourcesvmiGVR)+rest.SubResourcePath("addvolume")).
			To(subresourceApp.VMIAddVolumeRequestHandler).
			Reads(v1.AddVolumeOptions{}).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation(version.Version+"vmi-addvolume").
			Doc("Add a volume and disk to a running Virtual Machine Instance").
			Returns(http.StatusOK, "OK", "").
			Returns(http.StatusBadRequest, httpStatusBadRequestMessage, ""))

		subws.Route(subws.PUT(rest.ResourcePath(subresourcesvmiGVR)+rest.SubResourcePath("removevolume")).
			To(subresourceApp.VMIRemoveVolumeRequestHandler).
			Reads(v1.RemoveVolumeOptions{}).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation(version.Version+"vmi-removevolume").
			Doc("Removes a volume and disk from a running Virtual Machine Instance").
			Returns(http.StatusOK, "OK", "").
			Returns(http.StatusBadRequest, httpStatusBadRequestMessage, ""))

		subws.Route(subws.PUT(rest.ResourcePath(subresourcesvmGVR)+rest.SubResourcePath("addvolume")).
			To(subresourceApp.VMAddVolumeRequestHandler).
			Reads(v1.AddVolumeOptions{}).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation(version.Version+"vm-addvolume").
			Doc("Add a volume and disk to a running Virtual Machine.").
			Returns(http.StatusOK, "OK", "").
			Returns(http.StatusBadRequest, httpStatusBadRequestMessage, ""))

		subws.Route(subws.PUT(rest.ResourcePath(subresourcesvmGVR)+rest.SubResourcePath("removevolume")).
			To(subresourceApp.VMRemoveVolumeRequestHandler).
			Reads(v1.RemoveVolumeOptions{}).
			Param(rest.NamespaceParam(subws)).Param(rest.NameParam(subws)).
			Operation(version.Version+"vm-removevolume").
			Doc("Removes a volume and disk from a running Virtual Machine.").
			Returns(http.StatusOK, "OK", "").
			Returns(http.StatusBadRequest, httpStatusBadRequestMessage, ""))

		// Return empty api resource list.
		// K8s expects to be able to retrieve a resource list for each aggregated
		// app in order to discover what resources it provides. Without returning
		// an empty list here, there's a bug in the k8s resource discovery that
		// breaks kubectl's ability to reference short names for resources.
		subws.Route(subws.GET("/").
			Produces(restful.MIME_JSON).Writes(metav1.APIResourceList{}).
			To(func(request *restful.Request, response *restful.Response) {
				list := &metav1.APIResourceList{}

				list.Kind = "APIResourceList"
				list.GroupVersion = version.Group + "/" + version.Version
				list.APIVersion = version.Version
				list.APIResources = []metav1.APIResource{
					{
						Name:       "virtualmachineinstances/vnc",
						Namespaced: true,
					},
					{
						Name:       "virtualmachineinstances/console",
						Namespaced: true,
					},
					{
						Name:       "virtualmachineinstances/portforward",
						Namespaced: true,
					},
					{
						Name:       "virtualmachineinstances/pause",
						Namespaced: true,
					},
					{
						Name:       "virtualmachineinstances/unpause",
						Namespaced: true,
					},
					{
						Name:       "virtualmachineinstances/freeze",
						Namespaced: true,
					},
					{
						Name:       "virtualmachineinstances/unfreeze",
						Namespaced: true,
					},
					{
						Name:       "virtualmachines/start",
						Namespaced: true,
					},
					{
						Name:       "virtualmachines/stop",
						Namespaced: true,
					},
					{
						Name:       "virtualmachines/restart",
						Namespaced: true,
					},
					{
						Name:       "virtualmachines/migrate",
						Namespaced: true,
					},
					{
						Name:       "virtualmachineinstances/guestosinfo",
						Namespaced: true,
					},
					{
						Name:       "virtualmachineinstances/userlist",
						Namespaced: true,
					},
					{
						Name:       "virtualmachineinstances/filesystemlist",
						Namespaced: true,
					},
					{
						Name:       "virtualmachineinstances/addvolume",
						Namespaced: true,
					},
					{
						Name:       "virtualmachineinstances/removevolume",
						Namespaced: true,
					},
				}

				response.WriteAsJson(list)
			}).
			Operation(version.Version+"getAPISubResources").
			Doc("Get a KubeVirt API resources").
			Returns(http.StatusOK, "OK", metav1.APIResourceList{}).
			Returns(http.StatusNotFound, httpStatusNotFoundMessage, ""))

		restful.Add(subws)

		subwss = append(subwss, subws)
	}
	ws := new(restful.WebService)

	// K8s needs the ability to query the root paths
	ws.Route(ws.GET("/").
		Produces(restful.MIME_JSON).Writes(metav1.RootPaths{}).
		To(func(request *restful.Request, response *restful.Response) {
			paths := []string{"/apis",
				"/apis/",
				"/openapi/v2",
			}
			for _, version := range v1.SubresourceGroupVersions {
				paths = append(paths, rest.GroupBasePath(version))
				paths = append(paths, rest.GroupVersionBasePath(version))
			}
			response.WriteAsJson(&metav1.RootPaths{
				Paths: paths,
			})
		}).
		Operation("getRootPaths").
		Doc("Get KubeVirt API root paths").
		Returns(http.StatusOK, "OK", metav1.RootPaths{}).
		Returns(http.StatusNotFound, httpStatusNotFoundMessage, ""))
	ws.Route(ws.GET("/healthz").To(healthz.KubeConnectionHealthzFuncFactory(app.clusterConfig, apiHealthVersion)).Doc("Health endpoint"))

	for _, version := range v1.SubresourceGroupVersions {
		// K8s needs the ability to query info about a specific API group
		ws.Route(ws.GET(rest.GroupBasePath(version)).
			Produces(restful.MIME_JSON).Writes(metav1.APIGroup{}).
			To(func(request *restful.Request, response *restful.Response) {
				response.WriteAsJson(subresourceAPIGroup())
			}).
			Operation(version.Version+"GetSubAPIGroup").
			Doc("Get a KubeVirt API Group").
			Returns(http.StatusOK, "OK", metav1.APIGroup{}).
			Returns(http.StatusNotFound, httpStatusNotFoundMessage, ""))
	}

	// K8s needs the ability to query the list of API groups this endpoint supports
	ws.Route(ws.GET("apis").
		Produces(restful.MIME_JSON).Writes(metav1.APIGroupList{}).
		To(func(request *restful.Request, response *restful.Response) {
			list := &metav1.APIGroupList{}
			list.Kind = "APIGroupList"
			list.Groups = append(list.Groups, subresourceAPIGroup())
			response.WriteAsJson(list)
		}).
		Operation("getAPIGroupList").
		Doc("Get a KubeVirt API GroupList").
		Returns(http.StatusOK, "OK", metav1.APIGroupList{}).
		Returns(http.StatusNotFound, httpStatusNotFoundMessage, ""))

	once := sync.Once{}
	var openapispec *spec.Swagger
	ws.Route(ws.GET("openapi/v2").
		Consumes(restful.MIME_JSON).
		Produces(restful.MIME_JSON).
		To(func(request *restful.Request, response *restful.Response) {
			once.Do(func() {
				openapispec = openapi.LoadOpenAPISpec([]*restful.WebService{ws, subwss[0]})
				openapispec.Info.Version = virtversion.Get().String()
			})
			response.WriteAsJson(openapispec)
		}))

	restful.Add(ws)
}

func (app *virtAPIApp) Compose() {

	app.composeSubresources()

	restful.Filter(filter.RequestLoggingFilter())
	restful.Filter(restful.OPTIONSFilter())
	restful.Filter(func(req *restful.Request, resp *restful.Response, chain *restful.FilterChain) {
		allowed, reason, err := app.authorizor.Authorize(req)
		if err != nil {

			log.Log.Reason(err).Error("internal error during auth request")
			resp.WriteHeader(http.StatusInternalServerError)
			return
		} else if allowed {
			// request is permitted, so proceed with filter chain.
			chain.ProcessFilter(req, resp)
			return
		}
		resp.WriteErrorString(http.StatusUnauthorized, reason)
	})
}

func (app *virtAPIApp) ConfigureOpenAPIService() {
	spec := openapi.LoadOpenAPISpec(restful.RegisteredWebServices())
	config := openapi.CreateOpenAPIConfig(restful.RegisteredWebServices())
	ws := new(restful.WebService)
	ws.Path(config.APIPath)
	ws.Produces(restful.MIME_JSON)
	f := func(req *restful.Request, resp *restful.Response) {
		resp.WriteAsJson(spec)
	}
	ws.Route(ws.GET("/").To(f))

	restful.DefaultContainer.Add(ws)
	http.Handle("/swagger-ui/", http.StripPrefix("/swagger-ui/", http.FileServer(http.Dir(app.SwaggerUI))))
}

func deserializeStrings(in string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	var ret []string
	if err := json.Unmarshal([]byte(in), &ret); err != nil {
		return nil, err
	}
	return ret, nil
}

func (app *virtAPIApp) readRequestHeader() error {
	authConfigMap, err := app.virtCli.CoreV1().ConfigMaps(metav1.NamespaceSystem).Get(context.Background(), util.ExtensionAPIServerAuthenticationConfigMap, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// The request-header CA is mandatory. It can be retrieved from the configmap as we do here, or it must be provided
	// via flag on start of this apiserver. Since we don't do the latter, the former is mandatory for us
	// see https://github.com/kubernetes-incubator/apiserver-builder-alpha/blob/master/docs/concepts/auth.md#requestheader-authentication
	_, ok := authConfigMap.Data[util.RequestHeaderClientCAFileKey]
	if !ok {
		return fmt.Errorf("requestheader-client-ca-file not found in extension-apiserver-authentication ConfigMap")
	}

	// This config map also contains information about what
	// headers our authorizor should inspect
	headers, ok := authConfigMap.Data["requestheader-username-headers"]
	if ok {
		headerList, err := deserializeStrings(headers)
		if err != nil {
			return err
		}
		app.authorizor.AddUserHeaders(headerList)
	}

	headers, ok = authConfigMap.Data["requestheader-group-headers"]
	if ok {
		headerList, err := deserializeStrings(headers)
		if err != nil {
			return err
		}
		app.authorizor.AddGroupHeaders(headerList)
	}

	headers, ok = authConfigMap.Data["requestheader-extra-headers-prefix"]
	if ok {
		headerList, err := deserializeStrings(headers)
		if err != nil {
			return err
		}
		app.authorizor.AddExtraPrefixHeaders(headerList)
	}
	return nil
}

func (app *virtAPIApp) prepareCertManager() {
	app.certmanager = bootstrap.NewFileCertificateManager(app.tlsCertFilePath, app.tlsKeyFilePath)
	app.handlerCertManager = bootstrap.NewFileCertificateManager(app.handlerCertFilePath, app.handlerKeyFilePath)
}

func (app *virtAPIApp) registerValidatingWebhooks() {
	http.HandleFunc(components.VMICreateValidatePath, func(w http.ResponseWriter, r *http.Request) {
		validating_webhook.ServeVMICreate(w, r, app.clusterConfig)
	})
	http.HandleFunc(components.VMIUpdateValidatePath, func(w http.ResponseWriter, r *http.Request) {
		validating_webhook.ServeVMIUpdate(w, r, app.clusterConfig)
	})
	http.HandleFunc(components.VMValidatePath, func(w http.ResponseWriter, r *http.Request) {
		validating_webhook.ServeVMs(w, r, app.clusterConfig, app.virtCli)
	})
	http.HandleFunc(components.VMIRSValidatePath, func(w http.ResponseWriter, r *http.Request) {
		validating_webhook.ServeVMIRS(w, r, app.clusterConfig)
	})
	http.HandleFunc(components.VMIPresetValidatePath, func(w http.ResponseWriter, r *http.Request) {
		validating_webhook.ServeVMIPreset(w, r)
	})
	http.HandleFunc(components.MigrationCreateValidatePath, func(w http.ResponseWriter, r *http.Request) {
		validating_webhook.ServeMigrationCreate(w, r, app.clusterConfig, app.virtCli)
	})
	http.HandleFunc(components.MigrationUpdateValidatePath, func(w http.ResponseWriter, r *http.Request) {
		validating_webhook.ServeMigrationUpdate(w, r)
	})
	http.HandleFunc(components.VMSnapshotValidatePath, func(w http.ResponseWriter, r *http.Request) {
		validating_webhook.ServeVMSnapshots(w, r, app.clusterConfig, app.virtCli)
	})
	http.HandleFunc(components.VMRestoreValidatePath, func(w http.ResponseWriter, r *http.Request) {
		validating_webhook.ServeVMRestores(w, r, app.clusterConfig, app.virtCli)
	})
	http.HandleFunc(components.StatusValidatePath, func(w http.ResponseWriter, r *http.Request) {
		validating_webhook.ServeStatusValidation(w, r, app.clusterConfig, app.virtCli)
	})
	http.HandleFunc(components.LauncherEvictionValidatePath, func(w http.ResponseWriter, r *http.Request) {
		validating_webhook.ServePodEvictionInterceptor(w, r, app.clusterConfig, app.virtCli)
	})
}

func (app *virtAPIApp) registerMutatingWebhook() {

	http.HandleFunc(components.VMMutatePath, func(w http.ResponseWriter, r *http.Request) {
		mutating_webhook.ServeVMs(w, r, app.clusterConfig)
	})
	http.HandleFunc(components.VMIMutatePath, func(w http.ResponseWriter, r *http.Request) {
		mutating_webhook.ServeVMIs(w, r, app.clusterConfig)
	})
	http.HandleFunc(components.MigrationMutatePath, func(w http.ResponseWriter, r *http.Request) {
		mutating_webhook.ServeMigrationCreate(w, r)
	})
}

func (app *virtAPIApp) setupTLS(k8sCAManager webhooksutils.ClientCAManager, kubevirtCAManager webhooksutils.ClientCAManager) {

	// A VerifyClientCertIfGiven request means we're not guaranteed
	// a client has been authenticated unless they provide a peer
	// cert.
	//
	// Make sure to verify in subresource endpoint that peer cert
	// was provided before processing request. If the peer cert is
	// given on the connection, then we can be guaranteed that it
	// was signed by the client CA in our pool.
	//
	// There is another ClientAuth type called 'RequireAndVerifyClientCert'
	// We can't use this type here because during the aggregated api status
	// check it attempts to hit '/' on our api endpoint to verify an http
	// response is given. That status request won't send a peer cert regardless
	// if the TLS handshake requests it. As a result, the TLS handshake fails
	// and our aggregated endpoint never becomes available.
	app.tlsConfig = webhooksutils.SetupTLSWithCertManager(k8sCAManager, app.certmanager, tls.VerifyClientCertIfGiven)
	app.handlerTLSConfiguration = webhooksutils.SetupTLSForVirtHandlerClients(kubevirtCAManager, app.handlerCertManager, app.externallyManaged)
}

func (app *virtAPIApp) startTLS(informerFactory controller.KubeInformerFactory) error {

	errors := make(chan error)
	c := make(chan os.Signal, 1)

	signal.Notify(c, os.Interrupt,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	)

	authConfigMapInformer := informerFactory.ApiAuthConfigMap()
	kubevirtCAConfigInformer := informerFactory.KubeVirtCAConfigMap()

	k8sCAManager := webhooksutils.NewKubernetesClientCAManager(authConfigMapInformer.GetStore())
	kubevirtCAInformer := webhooksutils.NewCAManager(kubevirtCAConfigInformer.GetStore(), app.namespace, app.caConfigMapName)

	app.setupTLS(k8sCAManager, kubevirtCAInformer)

	app.Compose()

	http.Handle("/metrics", promhttp.Handler())
	server := &http.Server{
		Addr:      fmt.Sprintf("%s:%d", app.BindAddress, app.Port),
		TLSConfig: app.tlsConfig,
	}

	// start TLS server
	go func() {
		errors <- server.ListenAndServeTLS("", "")
	}()

	// start graceful shutdown handler
	go func() {
		s := <-c
		log.Log.Infof("Received signal %s, initiating graceful shutdown", s.String())

		// pause briefly to ensure the load balancer has had a chance to
		// remove this endpoint from rotation due to pod.DeletionTimestamp != nil
		// By pausing, we reduce the chance that the load balancer will attempt to
		// route new requests to the service after we've started the shutdown
		// procedure
		time.Sleep(5 * time.Second)

		// by default, server.Shutdown() waits indefinitely for all existing
		// connections to close. We need to give this a timeout to ensure the
		// shutdown will eventually complete.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer func() {
			cancel()
		}()

		// Shutdown means new connections are not permitted and it waits for existing
		// connections to terminate (up to the context timeout)
		server.Shutdown(ctx)
		// Shutdown forces any existing connections that persist after the shutdown
		// times out to be forced closed.
		server.Close()
	}()

	// wait for server to exit
	err := <-errors

	if err != nil && err != http.ErrServerClosed {
		// ErrServerClosed is an expected error during normal shutdown
		return err
	}
	return nil
}

func (app *virtAPIApp) Run() {
	host, err := os.Hostname()
	if err != nil {
		panic(fmt.Errorf("unable to get hostname: %v", err))
	}
	app.host = host

	// get client Cert
	err = app.readRequestHeader()
	if err != nil {
		panic(err)
	}

	// Get/Set selfsigned cert
	app.prepareCertManager()

	// Build webhook subresources
	app.registerMutatingWebhook()
	app.registerValidatingWebhooks()

	// Run informers for webhooks usage
	webhookInformers := webhooks.GetInformers()
	kubeInformerFactory := controller.NewKubeInformerFactory(app.virtCli.RestClient(), app.virtCli, app.aggregatorClient, app.namespace)
	configMapInformer := kubeInformerFactory.ConfigMap()
	crdInformer := kubeInformerFactory.CRD()
	authConfigMapInformer := kubeInformerFactory.ApiAuthConfigMap()
	kubevirtCAConfigInformer := kubeInformerFactory.KubeVirtCAConfigMap()
	kubeVirtInformer := kubeInformerFactory.KubeVirt()

	// Wire up health check trigger
	configMapInformer.SetWatchErrorHandler(func(r *cache.Reflector, err error) {
		apiHealthVersion.Clear()
		cache.DefaultWatchErrorHandler(r, err)
	})

	stopChan := make(chan struct{}, 1)
	defer close(stopChan)
	go webhookInformers.VMIInformer.Run(stopChan)
	go webhookInformers.VMIPresetInformer.Run(stopChan)
	go webhookInformers.NamespaceLimitsInformer.Run(stopChan)
	go webhookInformers.VMRestoreInformer.Run(stopChan)
	go kubeVirtInformer.Run(stopChan)
	go configMapInformer.Run(stopChan)
	go crdInformer.Run(stopChan)
	go authConfigMapInformer.Run(stopChan)
	go kubevirtCAConfigInformer.Run(stopChan)
	cache.WaitForCacheSync(stopChan,
		crdInformer.HasSynced,
		authConfigMapInformer.HasSynced,
		kubevirtCAConfigInformer.HasSynced,
		kubeVirtInformer.HasSynced,
		webhookInformers.VMIInformer.HasSynced,
		webhookInformers.VMIPresetInformer.HasSynced,
		webhookInformers.NamespaceLimitsInformer.HasSynced,
		configMapInformer.HasSynced)

	app.clusterConfig = virtconfig.NewClusterConfig(configMapInformer, crdInformer, kubeVirtInformer, app.namespace)
	app.clusterConfig.SetConfigModifiedCallback(app.shouldChangeLogVerbosity)

	go app.certmanager.Start()
	go app.handlerCertManager.Start()

	// start TLS server
	// tls server will only accept connections when fetching a certificate and internal configuration passed once
	err = app.startTLS(kubeInformerFactory)
	if err != nil {
		panic(err)
	}
}

// Update virt-api log verbosity on relevant config changes
func (app *virtAPIApp) shouldChangeLogVerbosity() {
	verbosity := app.clusterConfig.GetVirtAPIVerbosity(app.host)
	log.Log.SetVerbosityLevel(int(verbosity))
	log.Log.V(2).Infof("set log verbosity to %d", verbosity)
}

func (app *virtAPIApp) AddFlags() {
	app.InitFlags()

	app.AddCommonFlags()

	flag.StringVar(&app.SwaggerUI, "swagger-ui", "third_party/swagger-ui",
		"swagger-ui location")
	flag.BoolVar(&app.SubresourcesOnly, "subresources-only", false,
		"Only serve subresource endpoints")
	flag.IntVar(&app.consoleServerPort, "console-server-port", defaultConsoleServerPort,
		"The port virt-handler listens on for console requests")
	flag.StringVar(&app.caConfigMapName, "ca-configmap-name", defaultCAConfigMapName,
		"The name of configmap containing CA certificates to authenticate requests presenting client certificates with matching CommonName")
	flag.StringVar(&app.tlsCertFilePath, "tls-cert-file", defaultTlsCertFilePath,
		"File containing the default x509 Certificate for HTTPS")
	flag.StringVar(&app.tlsKeyFilePath, "tls-key-file", defaultTlsKeyFilePath,
		"File containing the default x509 private key matching --tls-cert-file")
	flag.StringVar(&app.handlerCertFilePath, "handler-cert-file", defaultHandlerCertFilePath,
		"Client certificate used to prove the identity of the virt-api when it must call virt-handler during a request")
	flag.StringVar(&app.handlerKeyFilePath, "handler-key-file", defaultHandlerKeyFilePath,
		"Private key for the client certificate used to prove the identity of the virt-api when it must call virt-handler during a request")
	flag.BoolVar(&app.externallyManaged, "externally-managed", false,
		"Allow intermediate certificates to be used in building up the chain of trust when certificates are externally managed")
}

// GetGsInfo returns the libguestfs-tools image information based on the KubeVirt installation in the namespace.
func (app *virtAPIApp) GetGsInfo() func(_ *restful.Request, response *restful.Response) {
	return func(_ *restful.Request, response *restful.Response) {
		var kvConfig virtoperatorutils.KubeVirtDeploymentConfig
		kv := app.clusterConfig.GetConfigFromKubeVirtCR()
		if kv == nil {
			error_guestfs(fmt.Errorf("Failed getting KubeVirt config"), response)
		}
		err := json.Unmarshal([]byte(kv.Status.ObservedDeploymentConfig), &kvConfig)
		if err != nil {
			error_guestfs(err, response)
			return
		}
		response.WriteAsJson(kubecli.GuestfsInfo{
			Registry: kv.Status.ObservedKubeVirtRegistry,
			Tag:      kv.Status.ObservedKubeVirtVersion,
			Digest:   kvConfig.GsSha,
		})
		return
	}
}

func error_guestfs(err error, response *restful.Response) {
	res := map[string]interface{}{}
	res["guestfs"] = map[string]interface{}{"status": "failed", "error": fmt.Sprintf("%v", err)}
	response.WriteHeaderAndJson(http.StatusInternalServerError, res, restful.MIME_JSON)
}
