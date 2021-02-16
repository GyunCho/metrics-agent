package kubernetes

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/cloudability/metrics-agent/retrieval/raw"
	"github.com/cloudability/metrics-agent/util"
	log "github.com/sirupsen/logrus"

	v1 "k8s.io/api/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// NodeSource is an interface to get a list of Nodes
type NodeSource interface {
	GetReadyNodes() ([]v1.Node, error)
	NodeAddress(node *v1.Node) (string, int32, error)
}

// ClientsetNodeSource implements NodeSource interface
type ClientsetNodeSource struct {
	clientSet kubernetes.Interface
}

type cadvisorStatsRequest struct {
	ContainerName string `json:"containerName,omitempty"`
	NumStats      int    `json:"num_stats,omitempty"`
	Subcontainers bool   `json:"subcontainers,omitempty"`
}

// NewClientsetNodeSource returns a ClientsetNodeSource with the given clientSet
func NewClientsetNodeSource(clientSet kubernetes.Interface) ClientsetNodeSource {
	return ClientsetNodeSource{
		clientSet: clientSet,
	}
}

// GetReadyNodes fetches the list of nodes from the clientSet and filters down to only ready nodes
func (cns ClientsetNodeSource) GetReadyNodes() ([]v1.Node, error) {
	allNodes, err := cns.clientSet.CoreV1().Nodes().List(metav1.ListOptions{})

	if err != nil {
		return nil, err
	}

	var readyNodes []v1.Node
	for _, n := range allNodes.Items {
		i, nc := getNodeCondition(
			&n.Status,
			v1.NodeReady)
		if i >= 0 && nc.Type == v1.NodeReady {
			readyNodes = append(readyNodes, n)
		} else {
			log.Debugf("node, %s, is in a notready state. Node Condition: %+v", n.Name, nc)
		}
	}

	if len(readyNodes) == 0 {
		return nil, fmt.Errorf("there were 0 nodes in a ready state")
	}

	if len(readyNodes) != len(allNodes.Items) {
		log.Info("some nodes were in a not ready state when retrieving nodes")
	}

	return readyNodes, nil
}

// NodeAddress returns the internal IP address and kubelet port of a given node
func (cns ClientsetNodeSource) NodeAddress(node *v1.Node) (string, int32, error) {
	// adapted from k8s.io/kubernetes/pkg/util/node
	for _, addr := range node.Status.Addresses {
		if addr.Type == v1.NodeInternalIP {
			return addr.Address, node.Status.DaemonEndpoints.KubeletEndpoint.Port, nil
		}
	}
	return "", 0, fmt.Errorf("Could not find internal IP address for node %s ", node.Name)
}

// Endpoint an enumeration representing the various metrics endpoints
type Endpoint uint8

const (
	// NodeStatsSummaryEndpoint the /stats/summary endpoint
	NodeStatsSummaryEndpoint Endpoint = iota

	// NodeContainerEndpoint the /stats/container endpoint
	NodeContainerEndpoint

	// NodeCadvisorEndpoint the /metrics/cadvisor/prometheus endpoint
	NodeCadvisorEndpoint
)

// EndpointMask a map representing the currently active endpoints.
// The keys of the map are the currently active endpoints.
type EndpointMask map[Endpoint]struct{}

// SetAvailable sets an endpoint availability state according to the supplied boolean
func (m EndpointMask) SetAvailable(endpoint Endpoint, available bool) {
	if available {
		m[endpoint] = struct{}{}
	} else {
		delete(m, endpoint)
	}
}

// Available gets the availability of an endpoint
func (m EndpointMask) Available(endpoint Endpoint) bool {
	_, ok := m[endpoint]
	return ok
}

func downloadNodeData(prefix string,
	config KubeAgentConfig,
	workDir *os.File,
	nodeSource NodeSource) (map[string]error, error) {

	var nodes []v1.Node

	failedNodeList := make(map[string]error)

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() (err error) {
		nodes, err = nodeSource.GetReadyNodes()
		return
	})
	if err != nil {
		return nil, fmt.Errorf("cloudability metric agent is unable to get a list of nodes: %v", err)
	}

	containersRequest, err := buildContainersRequest()

	if err != nil {
		return nil, fmt.Errorf("error occurred requesting container statistics: %v", err)
	}

	for _, n := range nodes {
		if n.Spec.ProviderID == "" {
			failedNodeList[n.Name] = errors.New("Provider ID for node does not exist. " +
				"If this condition persists it will cause inconsistent cluster allocation")
		}

		nd := nodeFetchData{
			nodeName:          n.Name,
			prefix:            prefix,
			workDir:           workDir,
			ClusterHostURL:    config.ClusterHostURL,
			containersRequest: containersRequest,
		}
		// retrieve node summary directly from node if possible and allowed.
		// The config shouldn't allow direct connection if Fargate nodes were
		// found in the cluster at startup, but check again here to be safe.
		if config.nodeRetrievalMethod == direct && !isFargateNode(n) {
			err := directNodeFetch(nodeSource, config, &n, nd)
			// no error, no need to try proxy
			if err == nil {
				continue
			}
			// make note of the error and fall through to proxy
			failedNodeList[n.Name] = fmt.Errorf("direct connect failed (will attempt proxy): %s", err)
		}
		// retrieve node summary via proxy
		err := proxyNodeFetch(nd, config)
		if err != nil {
			failedNodeList[n.Name] = fmt.Errorf("proxy connect failed: %s", err)
		}
	}

	return failedNodeList, nil
}

// nodeFetchData is a convenience wrapper for
// information used to fetch node stats and store
// in the appropriate file location
type nodeFetchData struct {
	nodeName          string
	prefix            string
	workDir           *os.File
	ClusterHostURL    string
	containersRequest []byte
}

// directNodeFetch retrieves node stats directly from the node api
func directNodeFetch(nodeSource NodeSource, config KubeAgentConfig, n *v1.Node, nd nodeFetchData) error {
	ip, port, err := nodeSource.NodeAddress(n)
	if err != nil {
		return fmt.Errorf("problem getting node address: %s", err)
	}
	d := directNodeEndpoints(ip, port)
	return retrieveNodeData(nd, config.NodeClient, config.DirectEndpointMask, d)
}

// proxyNodeFetch retrieves node data via the proxy api
func proxyNodeFetch(nd nodeFetchData, config KubeAgentConfig) error {
	proxy := proxyEndpoints(config.ClusterHostURL, nd.nodeName)
	return retrieveNodeData(nd, config.InClusterClient, config.ProxyEndpointMask, proxy)
}

type nodeAPI interface {
	statsSummary() string
	statsContainer() string
	mCAdvisor() string
}

type proxyAPI struct {
	clusterHostURL string
	nodeName       string
}

// statsSummary formats the proxy api stats/summary endpoint for the node
func (p proxyAPI) statsSummary() string {
	return fmt.Sprintf("%s/api/v1/nodes/%s/proxy/stats/summary", p.clusterHostURL, p.nodeName)
}

// statsContainer formats the proxy api stats/container endpoint for the node
func (p proxyAPI) statsContainer() string {
	return fmt.Sprintf("%s/api/v1/nodes/%s/proxy/stats/container/", p.clusterHostURL, p.nodeName)
}

// mCAdvisor formats the proxy api metrics/mCAdvisor endpoint, which outputs prometheus-format metrics
func (p proxyAPI) mCAdvisor() string {
	return fmt.Sprintf("%s/api/v1/nodes/%s/proxy/metrics/cadvisor", p.clusterHostURL, p.nodeName)
}

func proxyEndpoints(clusterHostURL, nodeName string) proxyAPI {
	return proxyAPI{
		clusterHostURL: clusterHostURL,
		nodeName:       nodeName,
	}
}

type directNode struct {
	ip   string
	port int64
}

// statsSummary formats the direct node stats/summary endpoint
func (d directNode) statsSummary() string {
	return fmt.Sprintf("https://%s:%v/stats/summary", d.ip, d.port)
}

// statsContainer formats the direct node stats/container endpoint
func (d directNode) statsContainer() string {
	return fmt.Sprintf("https://%s:%v/stats/container/", d.ip, d.port)
}

// mCAdvisor formats the direct node metrics/mCAdvisor endpoint
func (d directNode) mCAdvisor() string {
	return fmt.Sprintf("https://%s:%v/metrics/cadvisor", d.ip, d.port)
}

func directNodeEndpoints(ip string, port int32) directNode {
	return directNode{
		ip:   ip,
		port: int64(port),
	}
}

type sourceName struct {
	prefix   string
	nodeName string
}

func (s sourceName) summary() string {
	return fmt.Sprintf("%s-summary-%s", s.prefix, s.nodeName)
}

func (s sourceName) container() string {
	return fmt.Sprintf("%s-container-%s", s.prefix, s.nodeName)
}

func (s sourceName) cadvisorMetrics() string {
	return fmt.Sprintf("%s-cadvisor_metrics-%s", s.prefix, s.nodeName)
}

// retrieveNodeData fetches summary and container data from the node
func retrieveNodeData(nd nodeFetchData, c raw.Client, mask EndpointMask, api nodeAPI) error {
	source := sourceName{
		prefix:   nd.prefix,
		nodeName: nd.nodeName,
	}
	var err error

	if mask.Available(NodeStatsSummaryEndpoint) {
		// fetch stats/summary data
		log.Debug("Fetching data from /stats/summary endpoint")
		_, err = c.GetRawEndPoint(http.MethodGet, source.summary(), nd.workDir, api.statsSummary(), nil, true)
		if err != nil {
			return err
		}
	}

	if mask.Available(NodeCadvisorEndpoint) {
		// fetch metrics/mCAdvisor data
		log.Debug("Fetching data from /metrics/cadvisor endpoint")
		_, err = c.GetRawEndPoint(http.MethodGet, source.cadvisorMetrics(), nd.workDir, api.mCAdvisor(), nil, true)
		if err != nil {
			return err
		}
	}

	if mask.Available(NodeContainerEndpoint) {
		// fetch container details
		log.Debug("Fetching data from /stats/container endpoint")
		_, err = c.GetRawEndPoint(
			http.MethodPost, source.container(), nd.workDir, api.statsContainer(), nd.containersRequest, true)
		if err != nil {
			return err
		}
	}

	return err

}

//ensureNodeSource validates connectivity to the kubelet metrics endpoints.
// Attempts direct connection to the node summary & container stats endpoint
// if possible and allowed, otherwise attempts to connect via kube-proxy
func ensureNodeSource(config KubeAgentConfig) (KubeAgentConfig, error) {

	nodeHTTPClient := http.Client{
		Timeout: time.Second * 30,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				// nolint gosec
				InsecureSkipVerify: true,
			},
		}}

	clientSetNodeSource := NewClientsetNodeSource(config.Clientset)

	nodeClient := raw.NewClient(nodeHTTPClient, true, config.BearerToken, config.CollectionRetryLimit)

	config.NodeClient = nodeClient

	nodes, err := clientSetNodeSource.GetReadyNodes()
	if err != nil {
		return config, fmt.Errorf("error retrieving nodes: %s", err)
	}

	firstNode := &nodes[0]

	ip, port, err := clientSetNodeSource.NodeAddress(firstNode)
	if err != nil {
		return config, fmt.Errorf("error retrieving node addresses: %s", err)
	}

	if allowDirectConnect(config, nodes) {
		// test node direct connectivity
		d := directNodeEndpoints(ip, port)
		success, err := testNodeConn(config, &nodeHTTPClient, config.DirectEndpointMask, d.statsSummary(),
			d.statsContainer(), d.mCAdvisor())
		if err != nil {
			return config, err
		}
		if success {
			config.nodeRetrievalMethod = direct
			return config, nil
		}
	}

	// test node connectivity via kube-proxy
	p := proxyEndpoints(config.ClusterHostURL, firstNode.Name)
	success, err := testNodeConn(config, &config.HTTPClient, config.ProxyEndpointMask, p.statsSummary(),
		p.statsContainer(), p.mCAdvisor())
	if err != nil {
		return config, err
	}
	if success {
		config.NodeClient = raw.Client{}
		config.nodeRetrievalMethod = proxy
		return config, nil
	}

	config.nodeRetrievalMethod = unreachable
	config.RetrieveNodeSummaries = false
	return config, fmt.Errorf("unable to retrieve node metrics. Please verify RBAC roles: %v", err)
}

func testNodeConn(config KubeAgentConfig, client *http.Client, mask EndpointMask, nodeStatSum,
	containerStats, cadvisorMetrics string) (success bool, err error) {
	ns, _, err := util.TestHTTPConnection(client, nodeStatSum, http.MethodGet, config.BearerToken, 0, false)
	if err != nil {
		return false, err
	}
	log.Infof("Availability of the /stats/summary endpoint is: %v", ns)
	mask.SetAvailable(NodeStatsSummaryEndpoint, ns)

	cm, _, err := util.TestHTTPConnection(client, cadvisorMetrics, http.MethodGet, config.BearerToken, 0, false)
	if err != nil {
		return false, err
	}
	log.Infof("Availability of the /metrics/cadvisor endpoint is: %v", cm)
	mask.SetAvailable(NodeCadvisorEndpoint, cm)

	var cs = false
	if config.RetrieveStatsContainer {
		cs, _, err = util.TestHTTPConnection(client, containerStats, http.MethodPost, config.BearerToken, 0, false)
		if err != nil {
			return false, err
		}
		log.Infof("Availability of the /stats/container endpoint is: %v", cs)
		mask.SetAvailable(NodeContainerEndpoint, cs)
	}

	return ns && (cm || cs), nil
}

// isFargateNode detects whether a node is a Fargate node, which affects
// how the agent will connect to it
func isFargateNode(n v1.Node) bool {
	v := n.Labels["eks.amazonaws.com/compute-type"]
	if v == "fargate" {
		log.Debugf("Fargate node found: %s", n.Name)
		return true
	}
	return false
}

// allowDirectConnect determines whether the client and the
// type of nodes in the cluster will allow retrieving data directly
// from the node
func allowDirectConnect(config KubeAgentConfig, nodes []v1.Node) bool {
	if config.ForceKubeProxy {
		log.Infof("ForceKubeProxy is set, direct node connection disabled")
		return false
	}
	// Clusters may be mixed Fargate and non-Fargate.
	// To simplify handling, we disallow direct connection
	// if any Fargate nodes are found.
	for _, n := range nodes {
		if isFargateNode(n) {
			log.Infof("Fargate node found in cluster, direct node connection disabled. Learn more about Fargate support: %s", kbURL) //nolint: lll
			return false
		}
	}
	return true
}

func retrieveNodeSummaries(
	config KubeAgentConfig, msd string, metricSampleDir *os.File, nodeSource NodeSource) (err error) {

	config.failedNodeList = map[string]error{}

	// get node stats data
	config.failedNodeList, err = downloadNodeData("stats", config, metricSampleDir, nodeSource)
	if err != nil {
		return fmt.Errorf("error downloading node metrics: %s", err)
	}

	if len(config.failedNodeList) > 0 {
		log.Warnf("Warning failed to get node metrics: %+v", config.failedNodeList)
	}

	// move baseline metrics for each node into sample directory
	err = fetchNodeBaselines(msd, config.msExportDirectory.Name())
	if err != nil {
		return fmt.Errorf("error fetching node baseline files: %s", err)
	}

	// update node baselines with current sample
	err = updateNodeBaselines(msd, config.msExportDirectory.Name())
	if err != nil {
		return fmt.Errorf("error updating node baseline files: %s", err)
	}
	return nil
}

func buildContainersRequest() ([]byte, error) {
	// Request all containers.
	request := &cadvisorStatsRequest{
		Subcontainers: true,
		NumStats:      1,
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// getNodeCondition extracts the provided condition from the given status and returns that.
// Returns nil and -1 if the condition is not present, and the index of the located condition.
// Based on https://github.com/kubernetes/kubernetes/blob/v1.17.3/pkg/controller/util/node/controller_utils.go#L286
func getNodeCondition(status *v1.NodeStatus, conditionType v1.NodeConditionType) (int, *v1.NodeCondition) {
	if status == nil {
		return -1, nil
	}
	for i := range status.Conditions {
		if status.Conditions[i].Type == conditionType {
			return i, &status.Conditions[i]
		}
	}
	return -1, nil
}
