package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/jessevdk/go-flags"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/webdevops/go-common/prometheus/collector"
	"go.uber.org/zap"

	AzureDevops "github.com/webdevops/azure-devops-exporter/azure-devops-client"
	"github.com/webdevops/azure-devops-exporter/config"
)

const (
	Author = "webdevops.io"

	cacheTag = "v1"
)

var (
	argparser *flags.Parser
	opts      config.Opts

	AzureDevopsClient           *AzureDevops.AzureDevopsClient
	AzureDevopsServiceDiscovery *azureDevopsServiceDiscovery

	// Git version information
	gitCommit = "<unknown>"
	gitTag    = "<unknown>"
)

func main() {
	initLogger()
	initArgparser()

	logger.Infof("starting azure-devops-exporter v%s (%s; %s; by %v)", gitTag, gitCommit, runtime.Version(), Author)
	logger.Info(string(opts.GetJson()))

	logger.Infof("init AzureDevOps connection")
	initAzureDevOpsConnection()
	AzureDevopsServiceDiscovery = NewAzureDevopsServiceDiscovery()
	AzureDevopsServiceDiscovery.Update()

	logger.Info("init metrics collection")
	initMetricCollector()

	logger.Infof("starting http server on %s", opts.Server.Bind)
	startHttpServer()
}

// init argparser and parse/validate arguments
func initArgparser() {
	argparser = flags.NewParser(&opts, flags.Default)
	_, err := argparser.Parse()

	// check if there is an parse error
	if err != nil {
		var flagsErr *flags.Error
		if ok := errors.As(err, &flagsErr); ok && flagsErr.Type == flags.ErrHelp {
			os.Exit(0)
		} else {
			fmt.Println()
			argparser.WriteHelp(os.Stdout)
			os.Exit(1)
		}
	}

	// load accesstoken from file
	if opts.AzureDevops.AccessTokenFile != nil && len(*opts.AzureDevops.AccessTokenFile) > 0 {
		logger.Infof("reading access token from file \"%s\"", *opts.AzureDevops.AccessTokenFile)
		// load access token from file
		if val, err := os.ReadFile(*opts.AzureDevops.AccessTokenFile); err == nil {
			opts.AzureDevops.AccessToken = strings.TrimSpace(string(val))
		} else {
			logger.Fatalf("unable to read access token file \"%s\": %v", *opts.AzureDevops.AccessTokenFile, err)
		}
	}

	if len(opts.AzureDevops.AccessToken) == 0 && (len(opts.Azure.TenantId) == 0 || len(opts.Azure.ClientId) == 0) {
		logger.Fatalf("neither an Azure DevOps PAT token nor client credentials (tenant ID, client ID) for service principal authentication have been provided")
	}

	// ensure query paths and projects are splitted by '@'
	if opts.AzureDevops.QueriesWithProjects != nil {
		queryError := false
		for _, query := range opts.AzureDevops.QueriesWithProjects {
			if strings.Count(query, "@") != 1 {
				fmt.Println("Query path '", query, "' is malformed; should be '<query UUID>@<project UUID>'")
				queryError = true
			}
		}
		if queryError {
			os.Exit(1)
		}
	}

	// use default scrape time if null
	if opts.Scrape.TimeProjects == nil {
		opts.Scrape.TimeProjects = &opts.Scrape.Time
	}

	if opts.Scrape.TimeRepository == nil {
		opts.Scrape.TimeRepository = &opts.Scrape.Time
	}

	if opts.Scrape.TimePullRequest == nil {
		opts.Scrape.TimePullRequest = &opts.Scrape.Time
	}

	if opts.Scrape.TimeBuild == nil {
		opts.Scrape.TimeBuild = &opts.Scrape.Time
	}

	if opts.Scrape.TimeRelease == nil {
		opts.Scrape.TimeRelease = &opts.Scrape.Time
	}

	if opts.Scrape.TimeDeployment == nil {
		opts.Scrape.TimeDeployment = &opts.Scrape.Time
	}

	if opts.Scrape.TimeStats == nil {
		opts.Scrape.TimeStats = &opts.Scrape.Time
	}

	if opts.Scrape.TimeResourceUsage == nil {
		opts.Scrape.TimeResourceUsage = &opts.Scrape.Time
	}

	if opts.Stats.SummaryMaxAge == nil {
		opts.Stats.SummaryMaxAge = opts.Scrape.TimeStats
	}

	if opts.Scrape.TimeQuery == nil {
		opts.Scrape.TimeQuery = &opts.Scrape.Time
	}

	if opts.Scrape.TimeAgentPools == nil {
		opts.Scrape.TimeAgentPools = &opts.Scrape.Time
	}

	if v := os.Getenv("AZURE_DEVOPS_FILTER_AGENTPOOL"); v != "" {
		logger.Fatal("deprecated env var AZURE_DEVOPS_FILTER_AGENTPOOL detected, please use AZURE_DEVOPS_AGENTPOOL")
	}
}

// Init and build Azure authorzier
func initAzureDevOpsConnection() {
	AzureDevopsClient = AzureDevops.NewAzureDevopsClient(logger)
	if opts.AzureDevops.Url != nil {
		AzureDevopsClient.HostUrl = opts.AzureDevops.Url
	}

	logger.Infof("using organization: %v", opts.AzureDevops.Organisation)
	logger.Infof("using apiversion: %v", opts.AzureDevops.ApiVersion)
	logger.Infof("using concurrency: %v", opts.Request.ConcurrencyLimit)
	logger.Infof("using retries: %v", opts.Request.Retries)

	// ensure AZURE env vars are populated for azidentity
	if opts.Azure.TenantId != "" {
		if err := os.Setenv("AZURE_TENANT_ID", opts.Azure.TenantId); err != nil {
			panic(err)
		}
	}

	if opts.Azure.ClientId != "" {
		if err := os.Setenv("AZURE_CLIENT_ID", opts.Azure.ClientId); err != nil {
			panic(err)
		}
	}

	if opts.Azure.ClientSecret != "" {
		if err := os.Setenv("AZURE_CLIENT_SECRET", opts.Azure.ClientSecret); err != nil {
			panic(err)
		}
	}

	AzureDevopsClient.SetOrganization(opts.AzureDevops.Organisation)
	if opts.AzureDevops.AccessToken != "" {
		AzureDevopsClient.SetAccessToken(opts.AzureDevops.AccessToken)
	} else {
		if err := AzureDevopsClient.UseAzAuth(); err != nil {
			logger.Fatalf(err.Error())
		}
	}
	AzureDevopsClient.SetApiVersion(opts.AzureDevops.ApiVersion)
	AzureDevopsClient.SetConcurrency(opts.Request.ConcurrencyLimit)
	AzureDevopsClient.SetRetries(opts.Request.Retries)
	AzureDevopsClient.SetUserAgent(fmt.Sprintf("azure-devops-exporter/%v", gitTag))

	AzureDevopsClient.LimitProject = opts.Limit.Project
	AzureDevopsClient.LimitBuildsPerProject = opts.Limit.BuildsPerProject
	AzureDevopsClient.LimitBuildsPerDefinition = opts.Limit.BuildsPerDefinition
	AzureDevopsClient.LimitReleasesPerDefinition = opts.Limit.ReleasesPerDefinition
	AzureDevopsClient.LimitDeploymentPerDefinition = opts.Limit.DeploymentPerDefinition
	AzureDevopsClient.LimitReleaseDefinitionsPerProject = opts.Limit.ReleaseDefinitionsPerProject
	AzureDevopsClient.LimitReleasesPerProject = opts.Limit.ReleasesPerProject
}

func initMetricCollector() {
	startCollector := func(collectorName string, collectorType collector.ProcessorInterface, cacheFileName string, timeDuration *time.Duration) {
		if timeDuration.Seconds() > 0 {
			c := collector.New(collectorName, collectorType, logger)
			c.SetScapeTime(*timeDuration)
			c.SetCache(opts.GetCachePath(cacheFileName), collector.BuildCacheTag(cacheTag, opts.AzureDevops))
			if err := c.Start(); err != nil {
				logger.Fatal(err.Error())
			}
		} else {
			logger.With(zap.String("collector", collectorName)).Info("collector disabled")
		}
	}

	startCollector("Project", &MetricsCollectorProject{}, "project.json", opts.Scrape.TimeProjects)
	startCollector("AgentPool", &MetricsCollectorAgentPool{}, "agentpool.json", opts.Scrape.TimeAgentPools)
	startCollector("LatestBuild", &MetricsCollectorLatestBuild{}, "latestbuild.json", opts.Scrape.TimeBuild)
	startCollector("Repository", &MetricsCollectorRepository{}, "latestbuild.json", opts.Scrape.TimeRepository)
	startCollector("PullRequest", &MetricsCollectorPullRequest{}, "pullrequest.json", opts.Scrape.TimePullRequest)
	startCollector("Build", &MetricsCollectorBuild{}, "build.json", opts.Scrape.TimeBuild)
	startCollector("Release", &MetricsCollectorRelease{}, "release.json", opts.Scrape.TimeRelease)
	startCollector("Deployment", &MetricsCollectorDeployment{}, "deployment.json", opts.Scrape.TimeDeployment)
	startCollector("Stats", &MetricsCollectorStats{}, "stats.json", opts.Scrape.TimeStats)
	startCollector("ResourceUsage", &MetricsCollectorResourceUsage{}, "resourceusage.json", opts.Scrape.TimeResourceUsage)
	startCollector("Query", &MetricsCollectorQuery{}, "query.json", opts.Scrape.TimeQuery)
}

// start and handle prometheus handler
func startHttpServer() {
	mux := http.NewServeMux()

	// healthz
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if _, err := fmt.Fprint(w, "Ok"); err != nil {
			logger.Error(err)
		}
	})

	// readyz
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if _, err := fmt.Fprint(w, "Ok"); err != nil {
			logger.Error(err)
		}
	})

	mux.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{
		Addr:         opts.Server.Bind,
		Handler:      mux,
		ReadTimeout:  opts.Server.ReadTimeout,
		WriteTimeout: opts.Server.WriteTimeout,
	}
	logger.Fatal(srv.ListenAndServe())
}
