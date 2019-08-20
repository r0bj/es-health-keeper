package main

import (
	"fmt"
	"time"
	"strings"
	"os/exec"
	"os"
	"strconv"
	"bytes"
	"path/filepath"
	"os/signal"
	"syscall"
	"io/ioutil"
	"regexp"
	"encoding/json"

	"gopkg.in/alecthomas/kingpin.v2"
	log "github.com/Sirupsen/logrus"
	"github.com/nightlyone/lockfile"
	"github.com/parnurzeal/gorequest"
	"gopkg.in/yaml.v2"
)

const (
	ver string = "0.01"
	logDateLayout string = "2006-01-02 15:04:05"
	systemdDateLayout string  = "Mon 2006-01-02 15:04:05 MST"
	allocationAllJSON string = `{"transient":{"cluster.routing.allocation.enable": "all"}}`
	loopInterval int = 20
	sshTimeout int = 120
	delayBetweenRestarts int = 3600
	lockFile string = "es-health-keeper.lock"
	slackConnectionTimeout int = 5
)

var (
	prometheusURL = kingpin.Flag("url", "prometheus URL").Default("http://127.0.0.1:9090").Short('u').String()
	prometheusQuery = kingpin.Flag("query", "prometheus query").Default("avg_over_time(up{job=\"elasticsearch\"}[20m])").String()
	prometheusQueryResultThreshold = kingpin.Flag("query-result-threshold", "prometheus query result threshold").Default("0.05").Float()
	prometheusQueryTimeout = kingpin.Flag("prometheus-query-timeout", "prometheus query timeout").Default("10").Int()
	ESQueryTimeout = kingpin.Flag("es-query-timeout", "prometheus query timeout").Default("60").Int()
	prometheusBasicAuthUser = kingpin.Flag("auth-user", "prometheus basic auth user").String()
	prometheusBasicAuthPassword = kingpin.Flag("auth-password", "prometheus basic auth password").String()
	configFile = kingpin.Flag("config-file", "path to config file").Default("/etc/es-health-keeper.yaml").String()
	verbose = kingpin.Flag("verbose", "verbose mode").Short('v').Bool()
	sshUser = kingpin.Flag("ssh-user", "ssh user").Default("es_manager").String()
	sshPort = kingpin.Flag("ssh-port", "ssh port").Default("22").Short('p').Int()
	slackURL = kingpin.Flag("slack-url", "slack URL").Default("http://127.0.0.1").String()
	// slackChannel = kingpin.Flag("slack-channel", "slack channel to send messages").Default("#it-automatic-logs").String()
	slackChannel = kingpin.Flag("slack-channel", "slack channel to send messages").Default("#slack-test-hook").String()
	slackUsername = kingpin.Flag("slack-username", "slack username field").Default("es-health-keeper").String()
	slackIconEmoji = kingpin.Flag("slack-icon-emoji", "slack icon-emoji field").Default(":es-health-keeper:").String()
)

type PrometheusResult struct {
	Status string `json:"status"`
	Data struct {
		ResultType string `json:"resultType"`
		Result []MetricSerie `json:"result"`
	} `json:"data"`
}

type MetricSerie struct {
	Metric struct {
		Instance string `json:"instance"`
		Job string `json:"job"`
	} `json:"metric"`
	Value []interface{} `json:"value"`
}

type HTTPResponse struct {
	body string
	err error
}

type CommandResult struct {
	host string
	service string
	stdout string
	err error
}

type HostCommandsResult struct {
	combinedErr error
	commandResults []CommandResult
}

// type Config struct {
// 	ElasticsearchClusters map[string]struct {
// 		URL string `yaml:"url"`
// 		Version string `yaml:"version"`
// 		Hosts map[string][]string `yaml:"hosts"`
// 	} `yaml:"elasticsearch_clusters"`
// }

type Config struct {
	ElasticsearchClusters map[string]ConfigCluster `yaml:"elasticsearch_clusters"`
}

type ConfigCluster struct {
	URL string `yaml:"url"`
	Version string `yaml:"version"`
	Hosts map[string][]string `yaml:"hosts"`
}

type ClusterHealth struct {
	Status string `json:"status"`
}

type ClusterSettings struct {
	Transient struct {
		Cluster struct {
			Routing struct {
				Allocation struct {
					Enable string `json:"enable"`
				} `json:"allocation"`
			} `json:"routing"`
		} `json:"cluster"`
	} `json:"transient"`
}

// Command : containts exec command data
type Command struct {
	cmd string
	args []string
}

// Payload : containts slack Payload data
type Payload struct {
	Username string `json:"username"`
	Channel string `json:"channel"`
	IconEmoji string `json:"icon_emoji"`
	Attachments []Attachment `json:"attachments"`
}

// Attachment : containts slack Attachment data
type Attachment struct {
	Color string `json:"color"`
	Text string `json:"text"`
	MrkdwnIn []string `json:"mrkdwn_in"`
}

func httpGet(url, basicAuthUser, basicAuthPassword string, response chan<- HTTPResponse) {
	var msg HTTPResponse

	request := gorequest.New()

	if basicAuthUser != "" && basicAuthPassword != "" {
		request = gorequest.New().SetBasicAuth(basicAuthUser, basicAuthPassword)
	}

	resp, body, errs := request.Get(url).End()

	if errs != nil {
		var errsStr []string
		for _, e := range errs {
			errsStr = append(errsStr, fmt.Sprintf("%s", e))
		}
		msg.err = fmt.Errorf("%s", strings.Join(errsStr, ", "))
		response <- msg
		return
	}

	if resp.StatusCode == 200 {
		msg.body = body
	} else {
		msg.err = fmt.Errorf("HTTP response code: %s", resp.Status)
	}
	response <- msg
}

func httpPut(url, data string, response chan<- error) {
	request := gorequest.New()
	request.Header.Set("Content-Type", "application/json")
	resp, _, errs := request.Put(url).Send(data).End()

	if errs != nil {
		var errsStr []string
		for _, e := range errs {
			errsStr = append(errsStr, fmt.Sprintf("%s", e))
		}
		response <- fmt.Errorf("%s", strings.Join(errsStr, ", "))
		return
	}

	if resp.StatusCode != 200 {
		response <- fmt.Errorf("HTTP response code: %s", resp.Status)
		return
	}

	response <- nil
}

func getPrometheusMetric(prometheusURL, prometheusBasicAuthUser, prometheusBasicAuthPassword, prometheusQuery string) (PrometheusResult, error) {
	response := make(chan HTTPResponse)
	go httpGet(prometheusURL + "/api/v1/query?query=" + prometheusQuery, prometheusBasicAuthUser, prometheusBasicAuthPassword, response)

	var prometheusResult PrometheusResult
	select {
	case msg := <-response:
		if msg.err == nil {
			err := json.Unmarshal([]byte(msg.body), &prometheusResult)
			if err != nil {
				return prometheusResult, err
			}
		} else {
			return prometheusResult, msg.err
		}
	case <-time.After(time.Second * time.Duration(*prometheusQueryTimeout)):
		return prometheusResult, fmt.Errorf("%s: prometheus connection timeout", prometheusURL)
	}

	return prometheusResult, nil
}

func getClusterStatus(esURL string) (ClusterHealth, error) {
	response := make(chan HTTPResponse)
	go httpGet(esURL + "/_cluster/health", "", "", response)

	var clusterHealth ClusterHealth
	select {
	case msg := <-response:
		if msg.err == nil {
			err := json.Unmarshal([]byte(msg.body), &clusterHealth)
			if err != nil {
				return clusterHealth, err
			}
		} else {
			return clusterHealth, msg.err
		}
	case <-time.After(time.Second * time.Duration(*ESQueryTimeout)):
		return clusterHealth, fmt.Errorf("%s: elasticsearch connection timeout", esURL)
	}

	return clusterHealth, nil
}

func getClusterAllocation(esURL string) (ClusterSettings, error) {
	response := make(chan HTTPResponse)
	go httpGet(esURL + "/_cluster/settings", "", "", response)

	var clusterSettings ClusterSettings
	select {
	case msg := <-response:
		if msg.err == nil {
			err := json.Unmarshal([]byte(msg.body), &clusterSettings)
			if err != nil {
				return clusterSettings, err
			}
		} else {
			return clusterSettings, msg.err
		}
	case <-time.After(time.Second * time.Duration(*ESQueryTimeout)):
		return clusterSettings, fmt.Errorf("%s: elasticsearch connection timeout", esURL)
	}

	return clusterSettings, nil
}

func setClusterAllocationAll(esURL string) error {
	response := make(chan error)
	go httpPut(esURL + "/_cluster/settings", allocationAllJSON, response)

	select {
	case err := <-response:
		return err
	case <-time.After(time.Second * time.Duration(*ESQueryTimeout)):
		return fmt.Errorf("%s: elasticsearch connection timeout", esURL)
	}

	return nil
}

func findFaultyESInstances(prometheusURL, prometheusBasicAuthUser, prometheusBasicAuthPassword, prometheusQuery string, prometheusQueryResultThreshold float64) ([]string, error) {
	prometheusResult, err := getPrometheusMetric(prometheusURL, prometheusBasicAuthUser, prometheusBasicAuthPassword, prometheusQuery)
	if err != nil {
		return []string{}, err
	}

	var result []string
	for _, v := range prometheusResult.Data.Result {
		stringValue := v.Value[1].(string)
		if floatValue, err := strconv.ParseFloat(stringValue, 64); err == nil {
		    if floatValue <= prometheusQueryResultThreshold {
		    	result = append(result, v.Metric.Instance)
		    }
		} else {
			log.Debugf("Cannot convert string %s to float", stringValue)
		}
	}

	return result, nil
}

func parseConfig(file string) (Config, error) {
	var config Config

	source, err := ioutil.ReadFile(file)
	if err == nil {
		err = yaml.Unmarshal([]byte(source), &config)
		if err != nil {
			return config, err
		}
	} else {
		return config, err
	}

	return config, nil
}

func executeHostCommands(host, sshUser string, sshPort int, partialCmd []string, services []string, results chan<- HostCommandsResult) {
	remoteResults := make(chan CommandResult, len(services))

	for _, service := range services {
		cmd := append(partialCmd, service)
		go executeRemoteCommand(host, sshUser, sshPort, service, cmd, remoteResults)
	}

	var errsStr []string
	var hostCommandsResult HostCommandsResult
	for i := 1; i <= len(services); i++ {
		select {
		case commandResult := <-remoteResults:
			hostCommandsResult.commandResults = append(hostCommandsResult.commandResults, commandResult)
			if commandResult.err != nil {
				errsStr = append(errsStr, fmt.Sprintf("%s: %s", host, commandResult.err))
			}
		case <-time.After(time.Second * time.Duration(sshTimeout)):
			errsStr = append(errsStr, fmt.Sprintf("%s: SSH timeout", host))
		}
	}

	if len(errsStr) > 0 {
		hostCommandsResult.combinedErr = fmt.Errorf("%s", strings.Join(errsStr, ", "))
		results <- hostCommandsResult
		return
	}

	results <- hostCommandsResult
}

func executeRemoteCommand(host, sshUser string, sshPort int, service string, cmd []string, results chan<- CommandResult) {
	var commandResult CommandResult
	commandResult.host = host
	commandResult.service = service

	stdout, err := executeCommand(prepareSSHCommands(host, sshUser, sshPort, cmd))
	if err != nil {
		commandResult.err = err
		results <- commandResult
		return
	}

	commandResult.stdout = stdout
	results <- commandResult
}

func areServicesRunningLongEnough(clusterName string, clusterData ConfigCluster, sshUser string, sshPort int) (bool, error) {
	results := make(chan HostCommandsResult, len(clusterData.Hosts))

	partialCmd := []string{"systemctl", "--no-pager", "show"}

	for host, services := range clusterData.Hosts {
		go executeHostCommands(host, sshUser, sshPort, partialCmd, services, results)
	}

	var hostsCommandsResult []HostCommandsResult
	for i := 1; i <= len(clusterData.Hosts); i++ {
		hostCommandsResult := <-results
		hostsCommandsResult = append(hostsCommandsResult, hostCommandsResult)
		if hostCommandsResult.combinedErr != nil {
			return false, hostCommandsResult.combinedErr
		}
	}

	for _, hostCommandsResult := range hostsCommandsResult {
		for _, commandResult := range hostCommandsResult.commandResults {
			r := regexp.MustCompile(`ActiveEnterTimestamp=([ a-zA-Z0-9:-]+)`)

			findStrResult := r.FindStringSubmatch(commandResult.stdout)
			if len(findStrResult) < 2 {
				return false, fmt.Errorf("Cannot find timestamp string in command output")
			}

			timestamp, err := time.Parse(systemdDateLayout, findStrResult[1])
			if err != nil {
				return false, err
			}

			runningTime := time.Now().Unix() - timestamp.Unix()
			if runningTime <= int64(delayBetweenRestarts) {
				log.Infof("%s (restarter): service %s on %s is running for %ds, less then delay between restarts threshold (%ds), skipping", clusterName, commandResult.service, commandResult.host, runningTime, delayBetweenRestarts)
				return false, nil
			}
		}
	}

	return true, nil
}

func stopServices(clusterName string, clusterData ConfigCluster, sshUser string, sshPort int) error {
	results := make(chan HostCommandsResult, len(clusterData.Hosts))

	partialCmd := []string{"echo", "sudo", "systemctl", "stop"}

	for host, services := range clusterData.Hosts {
		go executeHostCommands(host, sshUser, sshPort, partialCmd, services, results)
	}

	for i := 1; i <= len(clusterData.Hosts); i++ {
		hostCommandsResult := <-results
		if hostCommandsResult.combinedErr != nil {
			return hostCommandsResult.combinedErr
		}
	}

	return nil
}

func startServices(clusterName string, clusterData ConfigCluster, sshUser string, sshPort int) error {
	results := make(chan HostCommandsResult, len(clusterData.Hosts))

	partialCmd := []string{"echo", "sudo", "systemctl", "start"}

	for host, services := range clusterData.Hosts {
		go executeHostCommands(host, sshUser, sshPort, partialCmd, services, results)
	}

	for i := 1; i <= len(clusterData.Hosts); i++ {
		hostCommandsResult := <-results
		if hostCommandsResult.combinedErr != nil {
			return hostCommandsResult.combinedErr
		}
	}

	return nil
}

func workerRestarter(id int, jobs <-chan string, config Config, sshUser string, sshPort int) {
	log.Infof("Worker restarter %d started", id)
	for clusterName := range jobs {
		log.Infof("%s (restarter): worker restarter %d started job", clusterName, id)

		if clusterData, ok := config.ElasticsearchClusters[clusterName]; ok {
			log.Infof("%s (restarter): starting restart procudure: %s", clusterName, clusterData)

			log.Infof("%s (restarter): checking timestamp for running services", clusterName)
			servicesRunningLongEnough, err := areServicesRunningLongEnough(clusterName, clusterData, sshUser, sshPort)
			if err != nil {
				log.Errorf("%s (restarter): checking timestamp for running services failed: %s", clusterName, err)
				continue
			}

			if servicesRunningLongEnough {
				sendSlackMsg(
					*slackURL,
					*slackChannel,
					fmt.Sprintf("Elasticsearch cluster *%s*: low responsiveness detected, restarting cluster.", clusterName),
					*slackUsername,
					"warning",
					*slackIconEmoji,
					slackConnectionTimeout,
				)

				if err := stopServices(clusterName, clusterData, sshUser, sshPort); err == nil {
					log.Infof("%s (restarter): stopping services success", clusterName)
				} else {
					log.Errorf("%s (restarter): stopping services failed: %s", clusterName, err)
				}

				if err := startServices(clusterName, clusterData, sshUser, sshPort); err == nil {
					log.Infof("%s (restarter): starting services success", clusterName)
					sendSlackMsg(
						*slackURL,
						*slackChannel,
						fmt.Sprintf("Elasticsearch cluster *%s*: restarting cluster finished successfully.", clusterName),
						*slackUsername,
						"good",
						*slackIconEmoji,
						slackConnectionTimeout,
					)
				} else {
					log.Errorf("%s (restarter): starting services failed: %s", clusterName, err)
					sendSlackMsg(
						*slackURL,
						*slackChannel,
						fmt.Sprintf("Elasticsearch cluster *%s*: restarting cluster failed. Automatic recovery failed. Manual intervention required.", clusterName),
						*slackUsername,
						"danger",
						*slackIconEmoji,
						slackConnectionTimeout,
					)
				}
			}
		} else {
			log.Infof("%s (restarter): no data in config", clusterName)
		}
    }
}

func workerSettingsChanger(clusterName string, config Config) {
	log.Infof("%s: worker settings changer started", clusterName)
	for {
		if clusterData, ok := config.ElasticsearchClusters[clusterName]; ok {
			allocation, err := getClusterAllocation(clusterData.URL)
			if err == nil {
				if strings.ToLower(allocation.Transient.Cluster.Routing.Allocation.Enable) != "all" {
					log.Infof("%s: cluster allocation is not 'all'", clusterName)

					status, err := getClusterStatus(clusterData.URL)
					if err == nil {
						if strings.ToLower(status.Status) == "yellow" || strings.ToLower(status.Status) == "green" {
							log.Infof("%s: changing cluster allocation to 'all'", clusterName)

							err := setClusterAllocationAll(clusterData.URL)
							if err == nil {
								log.Infof("%s: cluster allocation changed to 'all'", clusterName)
							} else {
								log.Warnf("%s: cannot set cluster allocation to 'all'", clusterName)
							}
						} else {
							log.Debugf("%s: cluster status %s, skipping", clusterName, strings.ToLower(status.Status))
						}
					} else {
						log.Warnf("%s: cannot get cluster status", clusterName)
					}
				} else {
					log.Debugf("%s: cluster allocation 'all', skipping", clusterName)
				}
			} else {
				log.Warnf("%s: cannot get cluster allocation data", clusterName)
			}
		} else {
			log.Infof("%s: no data in config", clusterName)
		}

		time.Sleep(time.Second * time.Duration(loopInterval))
    }
}

func metricsMonitor(jobs chan<- string, prometheusURL, prometheusBasicAuthUser, prometheusBasicAuthPassword, prometheusQuery string, prometheusQueryResultThreshold float64) {	
	for {
		esInstances, err := findFaultyESInstances(prometheusURL, prometheusBasicAuthUser, prometheusBasicAuthPassword, prometheusQuery, prometheusQueryResultThreshold)
		if err == nil {
			for _, clusterName := range esInstances {
				jobs <- clusterName
			}
		} else {
			log.Error(err)
		}

		time.Sleep(time.Second * time.Duration(loopInterval))
	}
}

func prepareSSHCommands(host, sshUser string, sshPort int, remoteCmd []string) Command {
	var command Command
	command.cmd = "ssh"
	command.args = []string{
		"-o",
		"StrictHostKeyChecking=no",
		"-o",
		"PasswordAuthentication=no",
		"-p",
		strconv.Itoa(sshPort),
		"-l",
		sshUser,
		host,
	}
	command.args = append(command.args, remoteCmd...)

	return command
}

func executeCommand(command Command) (string, error) {
	cmd := exec.Command(command.cmd, command.args...)
	log.Debugf("Executing command: %v %v", command.cmd, strings.Join(command.args, " "))

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	if err != nil {
		return "", fmt.Errorf("%s: %s", err, stderr.String())
	}

	return stdout.String(), nil
}

func sendSlackMsg(webhookURL, channel, msg, username, color, iconEmoji string, timeout int) error {
	payload := Payload{
		Username: username,
		Channel: channel,
		IconEmoji: iconEmoji,
		Attachments: []Attachment{
			Attachment{
				Color: color,
				Text: msg,
				MrkdwnIn: []string{"text"},
			},
		},
	}

	json, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	result := make(chan error)
	go httpPost(webhookURL, string(json), result)

	select {
	case err := <-result:
		return err
	case <-time.After(time.Second * time.Duration(timeout)):
		return fmt.Errorf("%s: slack connection timeout", webhookURL)
	}
}

func httpPost(url, data string, result chan<- error) {
	request := gorequest.New()
	resp, _, errs := request.Post(url).Send(data).End()

	if errs != nil {
		var errsStr []string
		for _, e := range errs {
			errsStr = append(errsStr, fmt.Sprintf("%s", e))
		}
		result <- fmt.Errorf("%s", strings.Join(errsStr, ", "))
		return
	}

	if resp.StatusCode != 200 {
		result <- fmt.Errorf("HTTP response code: %s", resp.Status)
		return
	}

	result <- nil
}

func main() {
	customFormatter := new(log.TextFormatter)
	customFormatter.TimestampFormat = logDateLayout
	log.SetFormatter(customFormatter)
	customFormatter.FullTimestamp = true
	log.SetOutput(os.Stdout)

	kingpin.Version(ver)
	kingpin.Parse()

	if *verbose {
		log.SetLevel(log.DebugLevel)
	}

	log.Infof("Starting, version %s", ver)

	lock, err := lockfile.New(filepath.Join(os.TempDir(), lockFile))
	if err != nil {
		log.Fatalf("Cannot initialize lock. reason: %v", err)
	}

	if err := lock.TryLock(); err != nil {
		log.Fatalf("Cannot lock %v, reason: %v", lock, err)
	}
	defer lock.Unlock()

	go func() {
		sigchan := make(chan os.Signal, 1)
		signal.Notify(sigchan, syscall.SIGINT, syscall.SIGTERM)
		<-sigchan
		log.Error("Program killed!")

		lock.Unlock()
		os.Exit(1)
	}()

	config, err := parseConfig(*configFile)
	if err != nil {
		log.Fatal(err)
	}

	if len(config.ElasticsearchClusters) < 1 {
		log.Fatal("No data in config file")
	}


    jobs := make(chan string, len(config.ElasticsearchClusters))

    for w := 1; w <= len(config.ElasticsearchClusters); w++ {
        go workerRestarter(w, jobs, config, *sshUser, *sshPort)
    }

    for clusterName := range config.ElasticsearchClusters {
        go workerSettingsChanger(clusterName, config)
    }

    go metricsMonitor(jobs, *prometheusURL, *prometheusBasicAuthUser, *prometheusBasicAuthPassword, *prometheusQuery, *prometheusQueryResultThreshold)
    select {}
}
