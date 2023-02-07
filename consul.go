package consul

import (
	"flashcat.cloud/categraf/config"
	"flashcat.cloud/categraf/inputs"
	"flashcat.cloud/categraf/types"
	"fmt"
	"github.com/go-kit/log"
	consul_api "github.com/hashicorp/consul/api"
	"github.com/hashicorp/go-cleanhttp"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ./categraf --test --debug --interval 5 --inputs consul
const inputName = "consul"

type Consul struct {
	config.PluginConfig
	Instances []*Instance `toml:"instances"`
}

func init() {
	inputs.Add(inputName, func() inputs.Input {
		return &Consul{}
	})
}

type Exporter struct {
	client           *consul_api.Client
	queryOptions     consul_api.QueryOptions
	kvPrefix         string
	kvFilter         *regexp.Regexp
	healthSummary    bool
	logger           log.Logger
	requestLimitChan chan struct{}
}

func (pt *Consul) GetInstances() []inputs.Instance {
	ret := make([]inputs.Instance, len(pt.Instances))
	for i := 0; i < len(pt.Instances); i++ {
		ret[i] = pt.Instances[i]
	}
	return ret
}

type Instance struct {
	config.InstanceConfig
	ServerName   string        `toml:"server_name"`
	URI          string        `toml:"uri"`
	CAFile       string        `toml:"ca_file"`
	CertFile     string        `toml:"cert_file"`
	KeyFile      string        `toml:"keyfile"`
	Timeout      time.Duration `toml:"timeout"`
	Insecure     bool          `toml:"insecure"`
	RequestLimit int           `tome:"request_limit"`
}

func (ins *Instance) Init() error {
	if len(ins.URI) == 0 {
		return types.ErrInstancesEmpty
	}
	if ins.Timeout == 0 {
		ins.Timeout = time.Millisecond * 500
	}
	return nil
}

func New(ins Instance, queryOptions consul_api.QueryOptions, kvPrefix, kvFilter string, healthSummary bool, logger log.Logger) (*Exporter, error) {
	if len(ins.URI) == 0 {
		ins.URI = "http://localhost:8500"
	}
	uri := ins.URI
	if !strings.Contains(uri, "://") {
		uri = "http://" + uri
	}
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("invalid consul URL: %s", err)
	}
	if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid consul URL: %s", uri)
	}

	tlsConfig, err := consul_api.SetupTLSConfig(&consul_api.TLSConfig{
		Address:            ins.ServerName,
		CAFile:             ins.CAFile,
		CertFile:           ins.CertFile,
		KeyFile:            ins.KeyFile,
		InsecureSkipVerify: ins.Insecure,
	})
	if err != nil {
		return nil, err
	}
	transport := cleanhttp.DefaultPooledTransport()
	transport.TLSClientConfig = tlsConfig

	config := consul_api.DefaultConfig()
	config.Address = u.Host
	config.Scheme = u.Scheme
	if config.HttpClient == nil {
		config.HttpClient = &http.Client{}
	}
	config.HttpClient.Timeout = ins.Timeout
	config.HttpClient.Transport = transport

	client, err := consul_api.NewClient(config)
	if err != nil {
		return nil, err
	}

	var requestLimitChan chan struct{}
	//fmt.Println("RequestLimit :", ins.RequestLimit)
	if ins.RequestLimit > 0 {
		requestLimitChan = make(chan struct{}, ins.RequestLimit)
	}

	// Init our exporter.
	return &Exporter{
		client:           client,
		queryOptions:     queryOptions,
		kvPrefix:         kvPrefix,
		kvFilter:         regexp.MustCompile(kvFilter),
		healthSummary:    healthSummary,
		logger:           logger,
		requestLimitChan: requestLimitChan,
	}, nil
}

func (s *Instance) Gather(slist *types.SampleList) {

	exporter, err := New(*s, consul_api.QueryOptions{}, "", ".*", true, log.NewNopLogger())
	if err != nil {
		fmt.Println("err:", err)
	}

	ok := collectPeersMetric(*exporter, slist)
	collectLeaderMetric(*exporter, slist)
	collectNodesMetric(*exporter, slist)
	collectMembersMetric(*exporter, slist)
	collectMembersWanMetric(*exporter, slist)
	collectServicesMetric(*exporter, slist)
	collectHealthStateMetric(*exporter, slist)
	collectKeyValues(*exporter, slist)
	if ok {
		slist.PushSample(inputName, "up", 1)
	} else {
		slist.PushSample(inputName, "up", 0)
	}
}

func collectPeersMetric(e Exporter, slist *types.SampleList) bool {
	peers, err := e.client.Status().Peers()
	if err != nil {
		fmt.Println("collectPeersMetric err:", err)
		return false

	}
	//fmt.Println("collectPeersMetric:", peers)
	tags := map[string]string{}
	slist.PushSample(inputName, "raft_peers", peers, tags)

	return true
}

func collectLeaderMetric(e Exporter, slist *types.SampleList) {
	leader, err := e.client.Status().Leader()
	if err != nil {
		fmt.Println("collectLeaderMetric err:", err)

	}
	//fmt.Println("collectLeaderMetric:", leader)
	tags := map[string]string{}
	slist.PushSample(inputName, "raft_leader", leader, tags)
}

func collectNodesMetric(e Exporter, slist *types.SampleList) {
	nodes, _, err := e.client.Catalog().Nodes(&e.queryOptions)
	if err != nil {
		fmt.Println("collectNodesMetric err:", err)

	}
	//fmt.Println("collectNodesMetric:", len(nodes))
	tags := map[string]string{}
	slist.PushSample(inputName, "serf_lan_members", len(nodes), tags)

}

func collectMembersMetric(e Exporter, slist *types.SampleList) {
	members, err := e.client.Agent().Members(false)
	if err != nil {
		fmt.Println("collectMembersMetric err:", err)

	}
	for _, entry := range members {
		//fmt.Println("collectMembersMetric:", float64(entry.Status), entry.Name)
		tags := map[string]string{
			"member_name": entry.Name,
		}
		slist.PushSample(inputName, "serf_lan_member_status", float64(entry.Status), tags)

	}
}

func collectMembersWanMetric(e Exporter, slist *types.SampleList) {
	members, err := e.client.Agent().Members(true)
	if err != nil {
		fmt.Println("collectMembersWanMetric err:", err)

	}
	for _, entry := range members {
		//fmt.Println("collectMembersWanMetric:", float64(entry.Status), entry.Name, entry.Tags["dc"])
		tags := map[string]string{
			"member": entry.Name,
			"dc":     entry.Tags["dc"],
		}
		slist.PushSample(inputName, "serf_wan_member_status", float64(entry.Status), tags)
	}
}

func collectHealthStateMetric(e Exporter, slist *types.SampleList) {
	checks, _, err := e.client.Health().State("any", &e.queryOptions)
	if err != nil {
		fmt.Println("collectServicesMetric err:", err)
	}
	for _, hc := range checks {
		var passing, warning, critical, maintenance float64

		switch hc.Status {
		case consul_api.HealthPassing:
			passing = 1
		case consul_api.HealthWarning:
			warning = 1
		case consul_api.HealthCritical:
			critical = 1
		case consul_api.HealthMaint:
			maintenance = 1
		}

		if hc.ServiceID == "" {
			tags := map[string]string{
				"check":  hc.CheckID,
				"node":   hc.Node,
				"status": "passing",
			}
			//fmt.Println("collectHealthStateMetric:", passing, hc.CheckID, hc.Node, consul_api.HealthPassing)
			slist.PushSample(inputName, "health_node_status", passing, tags)
			tags = map[string]string{
				"check":  hc.CheckID,
				"node":   hc.Node,
				"status": "warning",
			}
			//fmt.Println("collectHealthStateMetric:", warning, hc.CheckID, hc.Node, consul_api.HealthWarning)
			slist.PushSample(inputName, "health_node_status", warning, tags)
			tags = map[string]string{
				"check":  hc.CheckID,
				"node":   hc.Node,
				"status": "critical",
			}
			//fmt.Println("collectHealthStateMetric:", critical, hc.CheckID, hc.Node, consul_api.HealthCritical)
			slist.PushSample(inputName, "health_node_status", critical, tags)
			tags = map[string]string{
				"check":  hc.CheckID,
				"node":   hc.Node,
				"status": "maintenance",
			}
			//fmt.Println("collectHealthStateMetric:", maintenance, hc.CheckID, hc.Node, consul_api.HealthMaint)
			slist.PushSample(inputName, "health_node_status", maintenance, tags)
		} else {
			tags := map[string]string{
				"check":      hc.CheckID,
				"node":       hc.Node,
				"status":     "passing",
				"service":    hc.ServiceName,
				"service_id": hc.ServiceID,
			}
			//fmt.Println("collectHealthStateMetric:", passing, hc.CheckID, hc.Node, hc.ServiceID, hc.ServiceName, consul_api.HealthPassing)
			slist.PushSample(inputName, "health_service_status", passing, tags)
			tags = map[string]string{
				"check":      hc.CheckID,
				"node":       hc.Node,
				"status":     "warning",
				"service":    hc.ServiceName,
				"service_id": hc.ServiceID,
			}
			//fmt.Println("collectHealthStateMetric:", warning, hc.CheckID, hc.Node, hc.ServiceID, hc.ServiceName, consul_api.HealthWarning)
			slist.PushSample(inputName, "health_service_status", warning, tags)
			tags = map[string]string{
				"check":      hc.CheckID,
				"node":       hc.Node,
				"status":     "critical",
				"service":    hc.ServiceName,
				"service_id": hc.ServiceID,
			}
			//fmt.Println("collectHealthStateMetric:", critical, hc.CheckID, hc.Node, hc.ServiceID, hc.ServiceName, consul_api.HealthCritical, )
			slist.PushSample(inputName, "health_service_status", critical, tags)
			tags = map[string]string{
				"check":      hc.CheckID,
				"node":       hc.Node,
				"status":     "maintenance",
				"service":    hc.ServiceName,
				"service_id": hc.ServiceID,
			}
			//fmt.Println("collectHealthStateMetric:", maintenance, hc.CheckID, hc.Node, hc.ServiceID, hc.ServiceName, consul_api.HealthMaint, )
			slist.PushSample(inputName, "health_service_status", maintenance, tags)
			//fmt.Println("collectHealthStateMetric:", 1, hc.ServiceID, hc.ServiceName, hc.CheckID, hc.Name, hc.Node, )
			etags := map[string]string{
				"check":      hc.CheckID,
				"check_name": hc.Name,
				"node":       hc.Node,
				"service_id": hc.ServiceID,
				"service":    hc.ServiceName,
			}
			slist.PushSample(inputName, "service_checks", 1, etags)
		}
	}

}

func collectServicesMetric(e Exporter, slist *types.SampleList) {
	serviceNames, _, err := e.client.Catalog().Services(&e.queryOptions)
	if err != nil {
		fmt.Println("collectServicesMetric err:", err)

	}
	//fmt.Println("collectServicesMetric:", serviceNames)
	if e.healthSummary {
		collectHealthSummary(serviceNames, e, slist)
	}
	//fmt.Println("collectServicesMetric over")
}

func collectHealthSummary(serviceNames map[string][]string, e Exporter, slist *types.SampleList) {
	ok := make(chan bool)

	for s := range serviceNames {
		go func(s string) {
			if e.requestLimitChan != nil {
				e.requestLimitChan <- struct{}{}
				defer func() {
					<-e.requestLimitChan
				}()
			}
			ok <- collectOneHealthSummary(s, e, slist)
		}(s)
	}

	allOK := true
	for range serviceNames {
		allOK = <-ok && allOK
	}
	close(ok)
	//fmt.Println("collectHealthSummary over")
	return

}

func collectOneHealthSummary(serviceName string, e Exporter, slist *types.SampleList) bool {
	// See https://github.com/hashicorp/consul/issues/1096.
	if strings.HasPrefix(serviceName, "/") {
		//fmt.Println("collectOneHealthSummary : Skipping service because it starts with a slash", serviceName)
		return true
	}
	//fmt.Println("collectOneHealthSummary 1 : Fetching health summary", serviceName)

	service, _, err := e.client.Health().Service(serviceName, "", false, &e.queryOptions)
	if err != nil {
		fmt.Println("collectOneHealthSummary err:", err)
		return false
	}

	for _, entry := range service {
		// We have a Node, a Service, and one or more Checks. Our
		// service-node combo is passing if all checks have a `status`
		// of "passing."
		passing := 1.
		for _, hc := range entry.Checks {
			if hc.Status != consul_api.HealthPassing {
				passing = 0
				break
			}
		}
		//fmt.Println("collectOneHealthSummary 2 :", passing, entry.Service.ID, entry.Node.Node, entry.Service.Service)
		tags := map[string]string{
			"service_id":   entry.Service.ID,
			"node":         entry.Node.Node,
			"service_name": entry.Service.Service,
		}
		slist.PushSample(inputName, "catalog_service_node_healthy", passing, tags)

		etags := make(map[string]struct{})
		for _, etag := range entry.Service.Tags {
			if _, ok := etags[etag]; ok {
				continue
			}
			//fmt.Println("collectOneHealthSummary 3 :", 1, entry.Service.ID, entry.Node.Node, etag)
			tags := map[string]string{
				"service_id":   entry.Service.ID,
				"node":         entry.Node.Node,
				"service_name": entry.Service.Service,
				"tag":          etag,
			}
			slist.PushSample(inputName, "service_tag", passing, tags)

			etags[etag] = struct{}{}
		}
	}

	//fmt.Println("collectOneHealthSummary over")
	return true
}

func collectKeyValues(e Exporter, slist *types.SampleList) {
	if e.kvPrefix == "" {
		return
	}

	kv := e.client.KV()
	pairs, _, err := kv.List(e.kvPrefix, &e.queryOptions)
	if err != nil {
		fmt.Println("collectKeyValues err:", err)

	}

	for _, pair := range pairs {
		if e.kvFilter.MatchString(pair.Key) {
			val, err := strconv.ParseFloat(string(pair.Value), 64)
			if err == nil {
				//fmt.Println("collectKeyValues:", val, pair.Key)
				tags := map[string]string{
					"key": pair.Key,
				}
				slist.PushSample(inputName, "catalog_kv", val, tags)
			}
		}
	}
}
