package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/argoproj/pkg/rand"

	"github.com/argoproj/argo-cd/v2/cmpserver/apiclient"
	"github.com/argoproj/argo-cd/v2/common"
	repoclient "github.com/argoproj/argo-cd/v2/reposerver/apiclient"
	"github.com/argoproj/argo-cd/v2/util/buffered_context"
	"github.com/argoproj/argo-cd/v2/util/cmp"
	"github.com/argoproj/argo-cd/v2/util/io/files"

	"github.com/argoproj/gitops-engine/pkg/utils/kube"
	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/mattn/go-zglob"
	log "github.com/sirupsen/logrus"
)

// cmpTimeoutBuffer is the amount of time before the request deadline to timeout server-side work. It makes sure there's
// enough time before the client times out to send a meaningful error message.
const cmpTimeoutBuffer = 100 * time.Millisecond

// Service implements ConfigManagementPluginService interface
type Service struct {
	initConstants CMPServerInitConstants
}

type CMPServerInitConstants struct {
	PluginConfig PluginConfig
}

// NewService returns a new instance of the ConfigManagementPluginService
func NewService(initConstants CMPServerInitConstants) *Service {
	return &Service{
		initConstants: initConstants,
	}
}

func (s *Service) Init(workDir string) error {
	err := os.RemoveAll(workDir)
	if err != nil {
		return fmt.Errorf("error removing workdir %q: %w", workDir, err)
	}
	err = os.MkdirAll(workDir, 0700)
	if err != nil {
		return fmt.Errorf("error creating workdir %q: %w", workDir, err)
	}
	return nil
}

func runCommand(ctx context.Context, command Command, warnOnEmptyOutput string, path string, env []string) (string, error) {
	if len(command.Command) == 0 {
		return "", fmt.Errorf("Command is empty")
	}
	cmd := exec.CommandContext(ctx, command.Command[0], append(command.Command[1:], command.Args...)...)

	cmd.Env = env
	cmd.Dir = path

	execId, err := rand.RandString(5)
	if err != nil {
		return "", err
	}
	logCtx := log.WithFields(log.Fields{"execID": execId})

	argsToLog := getCommandArgsToLog(cmd)
	logCtx.WithFields(log.Fields{"dir": cmd.Dir}).Info(argsToLog)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Make sure the command is killed immediately on timeout. https://stackoverflow.com/a/38133948/684776
	cmd.SysProcAttr = newSysProcAttr(true)

	start := time.Now()
	err = cmd.Start()
	if err != nil {
		return "", err
	}

	go func() {
		<-ctx.Done()
		// Kill by group ID to make sure child processes are killed. The - tells `kill` that it's a group ID.
		// Since we didn't set Pgid in SysProcAttr, the group ID is the same as the process ID. https://pkg.go.dev/syscall#SysProcAttr
		_ = sysCallKill(-cmd.Process.Pid)
	}()

	err = cmd.Wait()

	duration := time.Since(start)
	output := stdout.String()

	logCtx.WithFields(log.Fields{"duration": duration}).Debug(output)

	if err != nil {
		err := newCmdError(argsToLog, errors.New(err.Error()), strings.TrimSpace(stderr.String()))
		logCtx.Error(err.Error())
		return strings.TrimSuffix(output, "\n"), err
	}
	if len(output) == 0 {
		logCtx.WithFields(log.Fields{
			"stderr":  stderr.String(),
			"command": command,
		}).Warn(warnOnEmptyOutput)
	}

	return strings.TrimSuffix(output, "\n"), nil
}

// getCommandArgsToLog represents the given command in a way that we can copy-and-paste into a terminal
func getCommandArgsToLog(cmd *exec.Cmd) string {
	var argsToLog []string
	for _, arg := range cmd.Args {
		containsSpace := false
		for _, r := range arg {
			if unicode.IsSpace(r) {
				containsSpace = true
				break
			}
		}
		if containsSpace {
			// add quotes and escape any internal quotes
			argsToLog = append(argsToLog, strconv.Quote(arg))
		} else {
			argsToLog = append(argsToLog, arg)
		}
	}
	args := strings.Join(argsToLog, " ")
	return args
}

type CmdError struct {
	Args   string
	Stderr string
	Cause  error
}

func (ce *CmdError) Error() string {
	if ce.Stderr == "" {
		return fmt.Sprintf("`%v` failed %v", ce.Args, ce.Cause)
	}
	return fmt.Sprintf("`%v` failed %v: %s", ce.Args, ce.Cause, ce.Stderr)
}

func newCmdError(args string, cause error, stderr string) *CmdError {
	return &CmdError{Args: args, Stderr: stderr, Cause: cause}
}

// Environ returns a list of environment variables in name=value format from a list of variables
func environ(envVars []*apiclient.EnvEntry) []string {
	var environ []string
	for _, item := range envVars {
		if item != nil && item.Name != "" && item.Value != "" {
			environ = append(environ, fmt.Sprintf("%s=%s", item.Name, item.Value))
		}
	}
	return environ
}

// getTempDirMustCleanup creates a temporary directory and returns a cleanup function.
func getTempDirMustCleanup(baseDir string) (workDir string, cleanup func(), err error) {
	workDir, err = files.CreateTempDir(baseDir)
	if err != nil {
		return "", nil, fmt.Errorf("error creating temp dir: %w", err)
	}
	cleanup = func() {
		if err := os.RemoveAll(workDir); err != nil {
			log.WithFields(map[string]interface{}{
				common.SecurityField:    common.SecurityHigh,
				common.SecurityCWEField: common.SecurityCWEIncompleteCleanup,
			}).Errorf("Failed to clean up temp directory: %s", err)
		}
	}
	return workDir, cleanup, nil
}

type Stream interface {
	Recv() (*apiclient.AppStreamRequest, error)
	Context() context.Context
}

type GenerateManifestStream interface {
	Stream
	SendAndClose(response *apiclient.ManifestResponse) error
}

// GenerateManifest runs generate command from plugin config file and returns generated manifest files
func (s *Service) GenerateManifest(stream apiclient.ConfigManagementPluginService_GenerateManifestServer) error {
	return s.generateManifestGeneric(stream)
}

func (s *Service) generateManifestGeneric(stream GenerateManifestStream) error {
	ctx, cancel := buffered_context.WithEarlierDeadline(stream.Context(), cmpTimeoutBuffer)
	defer cancel()
	workDir, cleanup, err := getTempDirMustCleanup(common.GetCMPWorkDir())
	if err != nil {
		return fmt.Errorf("error creating workdir for manifest generation: %w", err)
	}
	defer cleanup()

	metadata, err := cmp.ReceiveRepoStream(ctx, stream, workDir, s.initConstants.PluginConfig.Spec.PreserveFileMode)
	if err != nil {
		return fmt.Errorf("plugin (%s) generate manifest error receiving stream for app (%s): %w", s.initConstants.PluginConfig.Metadata.Name, metadata.AppName, err)
	}

	appPath := filepath.Clean(filepath.Join(workDir, metadata.AppRelPath))
	if !strings.HasPrefix(appPath, workDir) {
		return fmt.Errorf("plugin (%s) app (%s) illegal appPath `%s`: out of workDir bound", s.initConstants.PluginConfig.Metadata.Name, metadata.AppName, appPath)
	}
	response, err := s.generateManifest(ctx, appPath, metadata)
	if err != nil {
		return fmt.Errorf("plugin (%s) error generating manifests for app (%s): %w", s.initConstants.PluginConfig.Metadata.Name, metadata.AppName, err)
	}
	err = stream.SendAndClose(response)
	if err != nil {
		return fmt.Errorf("plugin (%s) error sending manifest response for app (%s): %w", s.initConstants.PluginConfig.Metadata.Name, metadata.AppName, err)
	}
	return nil
}

// generateManifest runs generate command from plugin config file and returns generated manifest files
func (s *Service) generateManifest(ctx context.Context, appDir string, metadata *apiclient.ManifestRequestMetadata) (*apiclient.ManifestResponse, error) {
	if deadline, ok := ctx.Deadline(); ok {
		log.Infof("Generating manifests with deadline %v from now", time.Until(deadline))
	} else {
		log.Info("Generating manifests with no request-level timeout")
	}

	config := s.initConstants.PluginConfig

	env := append(os.Environ(), environ(metadata.GetEnv())...)
	if len(config.Spec.Init.Command) > 0 {
		_, err := runCommand(ctx, config.Spec.Init, fmt.Sprintf("Plugin (%s) init returned zero output (this is ok) for app (%s)", s.initConstants.PluginConfig.Metadata.Name, metadata.AppName), appDir, env)
		if err != nil {
			return &apiclient.ManifestResponse{}, err
		}
	}

	out, err := runCommand(ctx, config.Spec.Generate, fmt.Sprintf("Plugin (%s) command returned zero output (this is likely problematic) for app (%s)", s.initConstants.PluginConfig.Metadata.Name, metadata.AppName), appDir, env)
	if err != nil {
		return &apiclient.ManifestResponse{}, err
	}

	manifests, err := kube.SplitYAMLToString([]byte(out))
	if err != nil {
		sanitizedManifests := manifests
		if len(sanitizedManifests) > 1000 {
			sanitizedManifests = manifests[:1000]
		}
		log.Debugf("Plugin (%s) failed to split generated manifests for app (%s). Beginning of generated manifests: %q", s.initConstants.PluginConfig.Metadata.Name, metadata.AppName, sanitizedManifests)
		return &apiclient.ManifestResponse{}, err
	}

	return &apiclient.ManifestResponse{
		Manifests: manifests,
	}, err
}

type MatchRepositoryStream interface {
	Stream
	SendAndClose(response *apiclient.RepositoryResponse) error
}

// MatchRepository receives the application stream and checks whether
// its repository type is supported by the config management plugin
// server.
// The checks are implemented in the following order:
//  1. If spec.Discover.FileName is provided it finds for a name match in Applications files
//  2. If spec.Discover.Find.Glob is provided if finds for a glob match in Applications files
//  3. Otherwise it runs the spec.Discover.Find.Command
func (s *Service) MatchRepository(stream apiclient.ConfigManagementPluginService_MatchRepositoryServer) error {
	return s.matchRepositoryGeneric(stream)
}

func (s *Service) matchRepositoryGeneric(stream MatchRepositoryStream) error {
	bufferedCtx, cancel := buffered_context.WithEarlierDeadline(stream.Context(), cmpTimeoutBuffer)
	defer cancel()

	workDir, cleanup, err := getTempDirMustCleanup(common.GetCMPWorkDir())
	if err != nil {
		return fmt.Errorf("error creating workdir for repository matching: %w", err)
	}
	defer cleanup()

	metadata, err := cmp.ReceiveRepoStream(bufferedCtx, stream, workDir, s.initConstants.PluginConfig.Spec.PreserveFileMode)
	if err != nil {
		return fmt.Errorf("match repository error receiving stream: %w", err)
	}

	isSupported, isDiscoveryEnabled, err := s.matchRepository(bufferedCtx, workDir, metadata)
	if err != nil {
		return fmt.Errorf("match repository error: %w", err)
	}
	repoResponse := &apiclient.RepositoryResponse{IsSupported: isSupported, IsDiscoveryEnabled: isDiscoveryEnabled}

	err = stream.SendAndClose(repoResponse)
	if err != nil {
		return fmt.Errorf("error sending match repository response: %w", err)
	}
	return nil
}

func (s *Service) matchRepository(ctx context.Context, workdir string, metadata *apiclient.ManifestRequestMetadata) (isSupported bool, isDiscoveryEnabled bool, err error) {
	config := s.initConstants.PluginConfig

	appRelPath := metadata.GetAppRelPath()
	logCtx := log.WithFields(log.Fields{
		"PluginName": s.initConstants.PluginConfig.Metadata.Name,
		"AppName":    metadata.AppName,
		"appRelPath": appRelPath,
	})
	appPath, err := securejoin.SecureJoin(workdir, appRelPath)
	if err != nil {
		logCtx.WithFields(map[string]interface{}{
			common.SecurityField:    common.SecurityHigh,
			common.SecurityCWEField: common.SecurityCWEIncompleteCleanup,
			"workdir":               workdir,
			"err":                   err,
		}).Errorf("error joining workdir and appRelPath")
	}

	if config.Spec.Discover.FileName != "" {
		pattern := filepath.Join(appPath, config.Spec.Discover.FileName)
		logCtx.WithFields(log.Fields{
			"fileName": config.Spec.Discover.FileName,
		}).Debug("config.Spec.Discover.FileName is provided")
		matches, err := filepath.Glob(pattern)
		if err != nil {
			logCtx.WithFields(log.Fields{
				"pattern": pattern,
				"err":     err,
			}).Debug("error finding filename match for pattern")
			return false, true, fmt.Errorf("plugin (%s) app (%s) error finding filename match for pattern %q: %w", s.initConstants.PluginConfig.Metadata.Name, metadata.AppName, pattern, err)
		}
		return len(matches) > 0, true, nil
	}

	if config.Spec.Discover.Find.Glob != "" {
		logCtx.Debug("config.Spec.Discover.Find.Glob is provided")
		pattern := filepath.Join(appPath, config.Spec.Discover.Find.Glob)
		// filepath.Glob doesn't have '**' support hence selecting third-party lib
		// https://github.com/golang/go/issues/11862
		matches, err := zglob.Glob(pattern)
		if err != nil {
			logCtx.WithFields(log.Fields{
				"pattern": pattern,
				"err":     err,
			}).Debug("error finding glob match for pattern")
			return false, true, fmt.Errorf("plugin (%s) app (%s) error finding glob match for pattern %q: %w", s.initConstants.PluginConfig.Metadata.Name, metadata.AppName, pattern, err)
		}

		return len(matches) > 0, true, nil
	}

	if len(config.Spec.Discover.Find.Command.Command) > 0 {
		logCtx.Debug("Going to try runCommand.")
		env := append(os.Environ(), environ(metadata.GetEnv())...)
		find, err := runCommand(ctx, config.Spec.Discover.Find.Command, fmt.Sprintf("Plugin (%s) discover reported it does not support this app (%s)", s.initConstants.PluginConfig.Metadata.Name, metadata.AppName), appPath, env)
		if err != nil {
			return false, true, fmt.Errorf("plugin (%s) app (%s) error running find command: %w", s.initConstants.PluginConfig.Metadata.Name, metadata.AppName, err)
		}
		return find != "", true, nil
	}

	return false, false, nil
}

// ParametersAnnouncementStream defines an interface able to send/receive a stream of parameter announcements.
type ParametersAnnouncementStream interface {
	Stream
	SendAndClose(response *apiclient.ParametersAnnouncementResponse) error
}

// GetParametersAnnouncement gets parameter announcements for a given Application and repo contents.
func (s *Service) GetParametersAnnouncement(stream apiclient.ConfigManagementPluginService_GetParametersAnnouncementServer) error {
	bufferedCtx, cancel := buffered_context.WithEarlierDeadline(stream.Context(), cmpTimeoutBuffer)
	defer cancel()

	workDir, cleanup, err := getTempDirMustCleanup(common.GetCMPWorkDir())
	if err != nil {
		return fmt.Errorf("plugin (%s) error creating workdir for generating parameter announcements: %w", s.initConstants.PluginConfig.Metadata.Name, err)
	}
	defer cleanup()

	metadata, err := cmp.ReceiveRepoStream(bufferedCtx, stream, workDir, s.initConstants.PluginConfig.Spec.PreserveFileMode)
	if err != nil {
		return fmt.Errorf("plugin (%s) parameters announcement error receiving stream: %w", s.initConstants.PluginConfig.Metadata.Name, err)
	}
	log.Info(metadata.AppName)
	appPath := filepath.Clean(filepath.Join(workDir, metadata.AppRelPath))
	if !strings.HasPrefix(appPath, workDir) {
		return fmt.Errorf("plugin (%s) app (%s) illegal appPath (%s): out of workDir bound (%s)", s.initConstants.PluginConfig.Metadata.Name, metadata.AppName, metadata.AppRelPath, workDir)
	}

	repoResponse, err := getParametersAnnouncement(bufferedCtx, appPath, s, metadata)
	if err != nil {
		return fmt.Errorf("plugin (%s) app (%s) get parameters announcement error: %w", s.initConstants.PluginConfig.Metadata.Name, metadata.AppName, err)
	}

	err = stream.SendAndClose(repoResponse)
	if err != nil {
		return fmt.Errorf("plugin (%s) app (%s) error sending parameters announcement response: %w", s.initConstants.PluginConfig.Metadata.Name, metadata.AppName, err)
	}
	return nil
}

func getParametersAnnouncement(ctx context.Context, appDir string, s *Service, metadata *apiclient.ManifestRequestMetadata) (*apiclient.ParametersAnnouncementResponse, error) {
	announcements := s.initConstants.PluginConfig.Spec.Parameters.Static
	augmentedAnnouncements := announcements
	command := s.initConstants.PluginConfig.Spec.Parameters.Dynamic
	if len(command.Command) > 0 {
		env := append(os.Environ(), environ(metadata.GetEnv())...)
		stdout, err := runCommand(ctx, command, fmt.Sprintf("Plugin (%s) dynamic parameter announcements failed to return json (e.g. []) for app (%s)", s.initConstants.PluginConfig.Metadata.Name, metadata.AppName), appDir, env)
		if err != nil {
			return nil, fmt.Errorf("plugin (%s) error executing dynamic parameter for app (%s) output command: %w", s.initConstants.PluginConfig.Metadata.Name, metadata.AppName, err)
		}

		var dynamicParamAnnouncements []*repoclient.ParameterAnnouncement
		err = json.Unmarshal([]byte(stdout), &dynamicParamAnnouncements)
		if err != nil {
			return nil, fmt.Errorf("plugin (%s) error unmarshaling dynamic parameter for app (%s) output into ParametersAnnouncementResponse: %w", s.initConstants.PluginConfig.Metadata.Name, metadata.AppName, err)
		}

		// dynamic goes first, because static should take precedence by being later.
		augmentedAnnouncements = append(dynamicParamAnnouncements, announcements...)
	}

	repoResponse := &apiclient.ParametersAnnouncementResponse{
		ParameterAnnouncements: augmentedAnnouncements,
	}
	return repoResponse, nil
}
