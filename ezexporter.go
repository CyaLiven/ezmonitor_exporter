package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/ini.v1"
	"gopkg.in/yaml.v2"
)

const (
	// metric的前缀名
	namespace = "ezmonitor"

	// 距离上次成功获取Time后2秒内再次运行则不判断更新时间是否变化
	interval = 2 * time.Second
)

// UpdateTime 记录monitor程序写入时间和exporter最后一次成功运行的时间
type UpdateTime struct {
	SuccessRunTime time.Time
	LastWriteTime  string
}

func (u *UpdateTime) ignoreCheckTime() bool {
	currentTime := time.Now()
	if currentTime.Sub(u.SuccessRunTime) <= interval {
		return true
	}
	return false
}

var (
	// platform: MTP-竞价撮合平台, ATP-综合业务平台, DTP-期权业务平台, ITP-港股通平台
	// type: ASHR-A股, BSHR-B股
	ezStausLabelNames = []string{"server", "platform", "type"}
	ezOptLabelNames   = []string{"server", "platform", "type", "pbu"}

	// 以监控文件名为key，记录最后更新时间
	lastUpdateTime = make(map[string]UpdateTime)
)

// newEZStatusMetric 读取监控文件中oesstatus段的内容
func newEZStatusMetric(metricName string, docString string, constLabels prometheus.Labels) *prometheus.Desc {
	return prometheus.NewDesc(prometheus.BuildFQName(namespace, "ezstatus", metricName), docString, ezStausLabelNames, constLabels)
}

// newEZOptMetric 读取监控文件中OperatorStatus.x段的内容
func newEZOptMetric(metricName string, docString string, constLabels prometheus.Labels) *prometheus.Desc {
	return prometheus.NewDesc(prometheus.BuildFQName(namespace, "operator", metricName), docString, ezOptLabelNames, constLabels)
}

type metrics map[string]*prometheus.Desc

var (
	ezStatusMetrics = metrics{
		"WarningLevel":  newEZStatusMetric("warning_level", "ez status.", nil),
		"OperatorTotal": newEZStatusMetric("operator_total", "OperatorTotal.", nil),
	}

	ezOptMetrics = metrics{
		"WarningLevel":    newEZOptMetric("warning_level", "operator status.", nil),
		"Status":          newEZOptMetric("pbu_status", "pbu status.", nil),
		"OrdNumSubmitted": newEZOptMetric("order_submit", "order submit number.", nil),
		"OrdNumConfirmed": newEZOptMetric("order_confirmed", "order submit  confirmed number.", nil),
		"TradeNumber":     newEZOptMetric("trade", "trade number.", nil),
		"Capability":      newEZOptMetric("capability", "pbu Capability", nil),
		"ProcessRate":     newEZOptMetric("process_rate", "the number of order confirmed from last minitue", nil),
		"MaxOrderNumber":  newEZOptMetric("max_order_num", "Max Order Number", nil),
	}

	ezUp = newEZStatusMetric("up", "Was the last scrape of ezoes or ezstep successful.", nil)
)

// configMap key:平台类型, value：文件列表(ASHR和BSHR最多各一个)
type configMap map[string][]string

// Exporter 从接口文件获取状态
type Exporter struct {
	mutex    sync.RWMutex
	ezConfig configMap
	ipAddr   string
}

// NewExporter return an initialized Exporter
func NewExporter(configFile string) (*Exporter, error) {
	ezConfig, err := parseConfigFile(configFile)
	if err != nil {
		return nil, err
	}

	// 取本机IP地址
	ipAddr, err := getIPAddr()
	if err != nil {
		return nil, err
	}

	return &Exporter{
		ezConfig: ezConfig,
		ipAddr:   ipAddr,
	}, nil
}

// Describe 接口实现
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	for _, m := range ezStatusMetrics {
		ch <- m
	}
	for _, m := range ezOptMetrics {
		ch <- m
	}

	ch <- ezUp
}

// Collect 接口实现
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	e.scrape(ch)
}

func (e *Exporter) scrape(ch chan<- prometheus.Metric) {
	for platform, statusFiles := range e.ezConfig {
		for _, statusFile := range statusFiles {
			Sections, err := getSections(statusFile)
			if err != nil {
				fmt.Printf("error [%s]: %s\n", statusFile, err)
				continue
			}

			oesStatusSection := Sections.Section("OESStatus")
			oesType := oesStatusSection.Key("OESType").String()

			ezTimeSection := Sections.Section("TIME")
			up := e.parseEZTimeSection(ezTimeSection, platform, oesType, ch, statusFile)
			if up != true {
				continue
			}

			e.parseEZStatusSection(oesStatusSection, platform, oesType, ch)

			operatorTotal, err := oesStatusSection.Key("OperatorTotal").Int()
			if err != nil {
				fmt.Printf("%s\n", err)
				continue
			}

			for id := 1; id <= operatorTotal; id++ {
				operatorSection := Sections.Section(fmt.Sprintf("OperatorStatus.%d", id))
				e.parseOptStatusSection(operatorSection, platform, oesType, ch)
			}

		}
	}

}

func (e *Exporter) parseEZStatusSection(section *ini.Section, platform string, oesType string, ch chan<- prometheus.Metric) {
	for metric := range ezStatusMetrics {
		value, _ := section.Key(metric).Float64()
		ch <- prometheus.MustNewConstMetric(ezStatusMetrics[metric], prometheus.GaugeValue, value, e.ipAddr, platform, oesType)
	}
}

func (e *Exporter) parseOptStatusSection(section *ini.Section, platform string, oesType string, ch chan<- prometheus.Metric) {
	var value float64
	operatorFullName := section.Key("Operator").String()
	pbu := operatorFullName[0:5]
	for metric := range ezOptMetrics {
		// 在EzStep中Capability需特殊处理，格式为 20/0
		if metric == "Capability" && platform != "MTP" {
			value = getCapabilityValue(section.Key(metric).String())
		} else {
			value, _ = section.Key(metric).Float64()
		}

		ch <- prometheus.MustNewConstMetric(ezOptMetrics[metric], prometheus.GaugeValue, value, e.ipAddr, platform, oesType, pbu)
	}
}

func (e *Exporter) parseEZTimeSection(section *ini.Section, platform string, oesType string, ch chan<- prometheus.Metric, statusFile string) bool {
	var value float64
	var updateTime UpdateTime

	monitorUpdateTime := section.Key("Time").String()
	updateTime = lastUpdateTime[statusFile]

	if updateTime.ignoreCheckTime() {
		value = 1
	} else {
		if monitorUpdateTime != updateTime.LastWriteTime {
			value = 1
			updateTime.LastWriteTime = monitorUpdateTime
			updateTime.SuccessRunTime = time.Now()
			lastUpdateTime[statusFile] = updateTime
			fmt.Printf("update[%s] updatetime:%s\n", statusFile, monitorUpdateTime)
		} else {
			fmt.Printf("error[%s] lastupdate:%s updatetime:%s\n", statusFile, lastUpdateTime[statusFile], updateTime)
		}
	}

	ch <- prometheus.MustNewConstMetric(ezUp, prometheus.GaugeValue, value, e.ipAddr, platform, oesType)

	if value == 1 {
		return true
	}
	return false
}

func getCapabilityValue(capability string) float64 {
	valueString := strings.Split(capability, "/")[0]
	value, _ := strconv.ParseFloat(valueString, 64)
	return value
}

func getIPAddr() (string, error) {
	addrs, err := net.InterfaceAddrs()

	if err != nil {
		return "", err
	}

	for _, address := range addrs {
		// 检查ip地址判断是否回环地址, 返回第一个非回环地址
		if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String(), nil
			}
		}
	}

	return "", fmt.Errorf("no ip address found")
}

func getSections(statusFile string) (*ini.File, error) {
	cfg, err := ini.Load(statusFile)
	// cfg.BlockMode = false
	if err != nil {
		return nil, fmt.Errorf("Fail to read file: %v", err)

	}

	return cfg, nil
}

type platformFiles struct {
	PlatForm string   `yaml:"platform"`
	Files    []string `yaml:"files"`
}

// MonitorFiles 监控的平台及文件列表
type MonitorFiles struct {
	PlatForms []platformFiles `yaml:"monitor_files"`
}

func parseConfigFile(configFile string) (configMap, error) {
	var statusFiles MonitorFiles

	config, err := ioutil.ReadFile(configFile)
	if err != nil {
		return nil, err
	}
	yaml.Unmarshal(config, &statusFiles)

	cm := make(configMap)
	for _, item := range statusFiles.PlatForms {
		// 获取每个平台所需监控的文件
		cm[item.PlatForm] = item.Files
	}

	// return make(configMap), nil
	return cm, nil
}

func getConfigFileAbsPath(configFile string) string {
	if configFile == "" {
		dir, _ := filepath.Abs(filepath.Dir(os.Args[0]))
		defaultFile, _ := filepath.Abs(filepath.Join(dir, "config.yml"))
		return defaultFile
	}
	return configFile
}

func main() {
	var (
		addr       = flag.String("listen address <ip>:<port>", ":8080", "The address to listen on for HTTP requests(default 0.0.0.0:8080).")
		configFile = flag.String("config file", "", "The config file path(default: config.yml)")
	)
	flag.Parse()

	configFileAbsPath := getConfigFileAbsPath(*configFile)

	metricsPath := "/metrics"

	exporter, err := NewExporter(configFileAbsPath)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(exporter)
	// 排除prometheus.DefaultGatherer
	newGatherers := prometheus.Gatherers{
		reg,
	}

	http.Handle(metricsPath, promhttp.HandlerFor(
		newGatherers,
		promhttp.HandlerOpts{
			ErrorHandling: promhttp.ContinueOnError,
		},
	))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
	    <head><title>ezmonitor Exporter</title></head>
	    <body>
	    <h1>ezmonitor Exporter</h1>
	    <p><a href='` + metricsPath + `'>Metrics</a></p>
	    </body>
	    </html>`))
	})

	fmt.Printf("Run at %s\n", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
