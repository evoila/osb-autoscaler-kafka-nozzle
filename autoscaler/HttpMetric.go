package autoscaler

type HttpMetric struct {
	Timestamp        int64  `json:"timestamp"`
	MetricName       string `json:"metricName"`
	AppId            string `json:"appId"`
	AppName          string `json:"appName"`
	Space            string `json:"space"`
	Organization     string `json:"organization"`
	OrganizationGuid string `json:"organizationGuid"`
	Requests         int32  `json:"requests"`
	Latency          int32  `json:"latency"`
	Description      string `json:"description"`
}
