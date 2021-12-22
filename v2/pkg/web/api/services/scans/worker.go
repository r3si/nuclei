package scans

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/projectdiscovery/gologger"
	"github.com/projectdiscovery/nuclei/v2/pkg/catalog"
	"github.com/projectdiscovery/nuclei/v2/pkg/catalog/loader"
	"github.com/projectdiscovery/nuclei/v2/pkg/core"
	"github.com/projectdiscovery/nuclei/v2/pkg/parsers"
	"github.com/projectdiscovery/nuclei/v2/pkg/progress"
	"github.com/projectdiscovery/nuclei/v2/pkg/protocols"
	"github.com/projectdiscovery/nuclei/v2/pkg/templates"
	"github.com/projectdiscovery/nuclei/v2/pkg/types"
	"github.com/projectdiscovery/nuclei/v2/pkg/web/api/services/settings"
	"go.uber.org/ratelimit"
	"gopkg.in/yaml.v3"
)

type PercentReturnFunc func() float64

func makePercentReturnFunc(stats progress.Progress) PercentReturnFunc {
	return PercentReturnFunc(func() float64 {
		return stats.Percent()
	})
}

// getSettingsForName gets settings for name and returns a types.Options structure
func (s *ScanService) getSettingsForName(name string) (*types.Options, error) {
	setting, err := s.db.GetSettingByName(context.Background(), name)
	if err != nil {
		return nil, err
	}
	settings := &settings.Settings{}
	if yamlErr := yaml.NewDecoder(strings.NewReader(setting.Settingdata)).Decode(settings); yamlErr != nil {
		return nil, yamlErr
	}
	typesOptions := settings.ToTypesOptions()
	return typesOptions, nil
}

func (s *ScanService) createExecuterOpts(scanID int64, templatesDirectory string, typesOptions *types.Options) (*scanContext, error) {
	// Use a no ticking progress service to track scan statistics
	progressImpl, _ := progress.NewStatsTicker(0, false, false, false, 0)
	s.Running.Store(scanID, makePercentReturnFunc(progressImpl))

	logWriter, err := s.Logs.Write(scanID)
	if err != nil {
		return nil, err
	}
	buflogWriter := bufio.NewWriter(logWriter)

	outputWriter := newWrappedOutputWriter(s.db, buflogWriter, scanID)

	executerOpts := protocols.ExecuterOptions{
		Output:       outputWriter,
		IssuesClient: nil, //todo: load from config value
		Options:      typesOptions,
		Progress:     progressImpl,
		Catalog:      catalog.New(templatesDirectory),
	}
	if typesOptions.RateLimitMinute > 0 {
		executerOpts.RateLimiter = ratelimit.New(typesOptions.RateLimitMinute, ratelimit.Per(60*time.Second))
	} else {
		executerOpts.RateLimiter = ratelimit.New(typesOptions.RateLimit)
	}
	scanContext := &scanContext{
		logs:         buflogWriter,
		logsFile:     logWriter,
		scanID:       scanID,
		scanService:  s,
		typesOptions: typesOptions,
		executerOpts: executerOpts,
	}
	return scanContext, nil
}

// scanContext contains context information for a scan
type scanContext struct {
	scanID       int64
	executer     *core.Engine
	store        *loader.Store
	logs         *bufio.Writer
	logsFile     io.WriteCloser
	typesOptions *types.Options
	scanService  *ScanService
	executerOpts protocols.ExecuterOptions
}

// Close closes the scan context performing cleanup operations
func (s *scanContext) Close() {
	s.logs.Flush()
	s.logsFile.Close()
	s.scanService.Running.Delete(s.scanID)

	gologger.Info().Msgf("[scans] [worker] [%d] Closed scan resources", s.scanID)
}

// createExecuterFromOpts creates executer from scanContext
func (s *ScanService) createExecuterFromOpts(scanCtx *scanContext) error {
	workflowLoader, err := parsers.NewLoader(&scanCtx.executerOpts)
	if err != nil {
		return err
	}
	scanCtx.executerOpts.WorkflowLoader = workflowLoader

	store, err := loader.New(loader.NewConfig(scanCtx.typesOptions, scanCtx.executerOpts.Catalog, scanCtx.executerOpts))
	if err != nil {
		return err
	}
	store.Load()
	scanCtx.store = store

	executer := core.New(scanCtx.typesOptions)
	executer.SetExecuterOptions(scanCtx.executerOpts)
	scanCtx.executer = executer
	return nil
}

// worker is a worker for executing a scan request
func (s *ScanService) worker(req ScanRequest) error {
	gologger.Info().Msgf("[scans] [worker] [%d] got new scan request", req.ScanID)

	typesOptions, err := s.getSettingsForName(req.Config)
	if err != nil {
		return err
	}
	gologger.Info().Msgf("[scans] [worker] [%d] loaded settings for config %s", req.ScanID, req.Config)

	templatesDirectory, templatesList, workflowsList, err := s.storeTemplatesFromRequest(req.Templates)
	if err != nil {
		return err
	}
	defer os.RemoveAll(templatesDirectory)

	gologger.Info().Msgf("[scans] [worker] [%d] loaded templates and workflows from req %v", req.ScanID, req.Templates)

	typesOptions.TemplatesDirectory = templatesDirectory
	typesOptions.Templates = templatesList
	typesOptions.Workflows = workflowsList

	scanCtx, err := s.createExecuterOpts(req.ScanID, templatesDirectory, typesOptions)
	if err != nil {
		return err
	}
	defer scanCtx.Close()

	err = s.createExecuterFromOpts(scanCtx)
	if err != nil {
		return err
	}

	var finalTemplates []*templates.Template
	finalTemplates = append(finalTemplates, scanCtx.store.Templates()...)
	finalTemplates = append(finalTemplates, scanCtx.store.Workflows()...)

	gologger.Info().Msgf("[scans] [worker] [%d] total loaded templates count: %d", req.ScanID, len(finalTemplates))

	inputProvider, err := s.inputProviderFromRequest(req.Targets)
	if err != nil {
		return err
	}
	gologger.Info().Msgf("[scans] [worker] [%d] total loaded input count: %d", req.ScanID, inputProvider.Count())

	scanCtx.executerOpts.Progress.Init(inputProvider.Count(), len(finalTemplates), int64(len(finalTemplates)*int(inputProvider.Count())))
	_ = scanCtx.executer.Execute(finalTemplates, inputProvider)

	gologger.Info().Msgf("[scans] [worker] [%d] finished scan for ID", req.ScanID)

	for k, v := range scanCtx.executerOpts.Progress.GetMetrics() {
		gologger.Info().Msgf("[scans] [worker] [%d] \tmetric '%s': %v", req.ScanID, k, v)
	}
	return nil
}
