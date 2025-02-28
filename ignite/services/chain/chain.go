package chain

import (
	"context"
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v5"

	"github.com/tendermint/spn/pkg/chainid"

	"github.com/ignite/cli/ignite/chainconfig"
	sperrors "github.com/ignite/cli/ignite/errors"
	"github.com/ignite/cli/ignite/pkg/chaincmd"
	chaincmdrunner "github.com/ignite/cli/ignite/pkg/chaincmd/runner"
	uilog "github.com/ignite/cli/ignite/pkg/cliui/log"
	"github.com/ignite/cli/ignite/pkg/confile"
	"github.com/ignite/cli/ignite/pkg/cosmosver"
	"github.com/ignite/cli/ignite/pkg/events"
	"github.com/ignite/cli/ignite/pkg/repoversion"
	"github.com/ignite/cli/ignite/pkg/xexec"
	"github.com/ignite/cli/ignite/pkg/xurl"
)

var appBackendSourceWatchPaths = []string{
	"app",
	"cmd",
	"x",
	"proto",
	"third_party",
}

type version struct {
	tag  string
	hash string
}

// Chain provides programatic access and tools for a Cosmos SDK blockchain.
type Chain struct {
	// app holds info about blockchain app.
	app App

	options chainOptions

	Version cosmosver.Version

	plugin         Plugin
	sourceVersion  version
	serveCancel    context.CancelFunc
	serveRefresher chan struct{}
	served         bool

	// protoBuiltAtLeastOnce indicates that app's proto generation at least made once.
	protoBuiltAtLeastOnce bool

	ev          events.Bus
	logOutputer uilog.Outputer
}

// chainOptions holds user given options that overwrites chain's defaults.
type chainOptions struct {
	// chainID is the chain's id.
	chainID string

	// homePath of the chain's config dir.
	homePath string

	// keyring backend used by commands if not specified in configuration
	keyringBackend chaincmd.KeyringBackend

	// isThirdPartyModuleCodegen indicates if proto code generation should be made
	// for 3rd party modules. SDK modules are also considered as a 3rd party.
	isThirdPartyModuleCodegenEnabled bool

	// checkDependencies checks that cached Go dependencies of the chain have not
	// been modified since they were downloaded.
	checkDependencies bool

	// printGeneratedPaths prints the output paths of the generated code
	printGeneratedPaths bool

	// path of a custom config file
	ConfigFile string
}

// Option configures Chain.
type Option func(*Chain)

// ID replaces chain's id with given id.
func ID(id string) Option {
	return func(c *Chain) {
		c.options.chainID = id
	}
}

// HomePath replaces chain's configuration home path with given path.
func HomePath(path string) Option {
	return func(c *Chain) {
		c.options.homePath = path
	}
}

// KeyringBackend specifies the keyring backend to use for the chain command
func KeyringBackend(keyringBackend chaincmd.KeyringBackend) Option {
	return func(c *Chain) {
		c.options.keyringBackend = keyringBackend
	}
}

// ConfigFile specifies a custom config file to use
func ConfigFile(configFile string) Option {
	return func(c *Chain) {
		c.options.ConfigFile = configFile
	}
}

// EnableThirdPartyModuleCodegen enables code generation for third party modules,
// including the SDK.
func EnableThirdPartyModuleCodegen() Option {
	return func(c *Chain) {
		c.options.isThirdPartyModuleCodegenEnabled = true
	}
}

// WithOutputer sets the CLI outputer for the chain.
func WithOutputer(s uilog.Outputer) Option {
	return func(c *Chain) {
		c.logOutputer = s
	}
}

// CollectEvents collects events from the chain.
func CollectEvents(ev events.Bus) Option {
	return func(c *Chain) {
		c.ev = ev
	}
}

// CheckDependencies checks that cached Go dependencies of the chain have not
// been modified since they were downloaded. Dependencies are checked by
// running `go mod verify`.
func CheckDependencies() Option {
	return func(c *Chain) {
		c.options.checkDependencies = true
	}
}

// PrintGeneratedPaths prints the output paths of the generated code.
func PrintGeneratedPaths() Option {
	return func(c *Chain) {
		c.options.printGeneratedPaths = true
	}
}

// New initializes a new Chain with options that its source lives at path.
func New(path string, options ...Option) (*Chain, error) {
	app, err := NewAppAt(path)
	if err != nil {
		return nil, err
	}

	c := &Chain{
		app:            app,
		serveRefresher: make(chan struct{}, 1),
	}

	// Apply the options
	for _, apply := range options {
		apply(c)
	}

	c.sourceVersion, err = c.appVersion()
	if err != nil && err != git.ErrRepositoryNotExists {
		return nil, err
	}

	c.Version, err = cosmosver.Detect(c.app.Path)
	if err != nil {
		return nil, err
	}

	if !c.Version.IsFamily(cosmosver.Stargate) {
		return nil, sperrors.ErrOnlyStargateSupported
	}

	// initialize the plugin depending on the version of the chain
	c.plugin = c.pickPlugin()

	return c, nil
}

func (c *Chain) appVersion() (v version, err error) {
	ver, err := repoversion.Determine(c.app.Path)
	if err != nil {
		return version{}, err
	}

	v.hash = ver.Hash
	v.tag = ver.Tag

	return v, nil
}

// RPCPublicAddress points to the public address of Tendermint RPC, this is shared by
// other chains for relayer related actions.
func (c *Chain) RPCPublicAddress() (string, error) {
	rpcAddress := os.Getenv("RPC_ADDRESS")
	if rpcAddress == "" {
		conf, err := c.Config()
		if err != nil {
			return "", err
		}
		validator := conf.Validators[0]
		servers, err := validator.GetServers()
		if err != nil {
			return "", err
		}
		rpcAddress = servers.RPC.Address
	}
	return rpcAddress, nil
}

// ConfigPath returns the config path of the chain
// Empty string means that the chain has no defined config
func (c *Chain) ConfigPath() string {
	if c.options.ConfigFile != "" {
		return c.options.ConfigFile
	}
	path, err := chainconfig.LocateDefault(c.app.Path)
	if err != nil {
		return ""
	}
	return path
}

// Config returns the config of the chain
func (c *Chain) Config() (*chainconfig.Config, error) {
	configPath := c.ConfigPath()
	if configPath == "" {
		return chainconfig.DefaultConfig(), nil
	}
	return chainconfig.ParseFile(configPath)
}

// ID returns the chain's id.
func (c *Chain) ID() (string, error) {
	// chainID in App has the most priority.
	if c.options.chainID != "" {
		return c.options.chainID, nil
	}

	// otherwise uses defined in config.yml
	chainConfig, err := c.Config()
	if err != nil {
		return "", err
	}
	genid, ok := chainConfig.Genesis["chain_id"]
	if ok {
		return genid.(string), nil
	}

	// use app name by default.
	return c.app.N(), nil
}

// ChainID returns the default network chain's id.
func (c *Chain) ChainID() (string, error) {
	return chainid.NewGenesisChainID(c.Name(), 1), nil
}

// Name returns the chain's name
func (c *Chain) Name() string {
	return c.app.N()
}

// Binary returns the name of app's default (appd) binary.
func (c *Chain) Binary() (string, error) {
	conf, err := c.Config()
	if err != nil {
		return "", err
	}

	if conf.Build.Binary != "" {
		return conf.Build.Binary, nil
	}

	return c.app.D(), nil
}

// SetHome sets the chain home directory.
func (c *Chain) SetHome(home string) {
	c.options.homePath = home
}

// Home returns the blockchain node's home dir.
func (c *Chain) Home() (string, error) {
	// check if home is explicitly defined for the app
	home := c.options.homePath
	if home == "" {
		// return default home otherwise
		var err error
		home, err = c.DefaultHome()
		if err != nil {
			return "", err
		}

	}

	// expand environment variables in home
	home = os.ExpandEnv(home)

	return home, nil
}

// DefaultHome returns the blockchain node's default home dir when not specified in the app
func (c *Chain) DefaultHome() (string, error) {
	// check if home is defined in config
	config, err := c.Config()
	if err != nil {
		return "", err
	}
	validator := config.Validators[0]
	if validator.Home != "" {
		return validator.Home, nil
	}

	return c.plugin.Home(), nil
}

// DefaultGentxPath returns default gentx.json path of the app.
func (c *Chain) DefaultGentxPath() (string, error) {
	home, err := c.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config/gentx/gentx.json"), nil
}

// GenesisPath returns genesis.json path of the app.
func (c *Chain) GenesisPath() (string, error) {
	home, err := c.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config/genesis.json"), nil
}

// GentxsPath returns the directory where gentxs are stored for the app.
func (c *Chain) GentxsPath() (string, error) {
	home, err := c.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config/gentx"), nil
}

// AppTOMLPath returns app.toml path of the app.
func (c *Chain) AppTOMLPath() (string, error) {
	home, err := c.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config/app.toml"), nil
}

// ConfigTOMLPath returns config.toml path of the app.
func (c *Chain) ConfigTOMLPath() (string, error) {
	home, err := c.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config/config.toml"), nil
}

// ClientTOMLPath returns client.toml path of the app.
func (c *Chain) ClientTOMLPath() (string, error) {
	home, err := c.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config/client.toml"), nil
}

// KeyringBackend returns the keyring backend chosen for the chain.
func (c *Chain) KeyringBackend() (chaincmd.KeyringBackend, error) {
	// 1st.
	if c.options.keyringBackend != "" {
		return c.options.keyringBackend, nil
	}

	config, err := c.Config()
	if err != nil {
		return "", err
	}

	// 2nd.
	validator := config.Validators[0]
	if validator.KeyringBackend != "" {
		return chaincmd.KeyringBackendFromString(validator.KeyringBackend)
	}

	// 3rd.
	if validator.Client != nil {
		if backend, ok := validator.Client["keyring-backend"]; ok {
			if backendStr, ok := backend.(string); ok {
				return chaincmd.KeyringBackendFromString(backendStr)
			}
		}
	}

	// 4th.
	configTOMLPath, err := c.ClientTOMLPath()
	if err != nil {
		return "", err
	}
	cf := confile.New(confile.DefaultTOMLEncodingCreator, configTOMLPath)
	var conf struct {
		KeyringBackend string `toml:"keyring-backend"`
	}
	if err := cf.Load(&conf); err != nil {
		return "", err
	}
	if conf.KeyringBackend != "" {
		return chaincmd.KeyringBackendFromString(conf.KeyringBackend)
	}

	// 5th.
	return chaincmd.KeyringBackendTest, nil
}

// Commands returns the runner execute commands on the chain's binary
func (c *Chain) Commands(ctx context.Context) (chaincmdrunner.Runner, error) {
	id, err := c.ID()
	if err != nil {
		return chaincmdrunner.Runner{}, err
	}

	home, err := c.Home()
	if err != nil {
		return chaincmdrunner.Runner{}, err
	}

	binary, err := c.Binary()
	if err != nil {
		return chaincmdrunner.Runner{}, err
	}

	// Try to make the binary path absolute. This will also
	// find the binary path when the Go bin path is not part
	// of the PATH environment variable.
	binary = xexec.TryResolveAbsPath(binary)

	backend, err := c.KeyringBackend()
	if err != nil {
		return chaincmdrunner.Runner{}, err
	}

	config, err := c.Config()
	if err != nil {
		return chaincmdrunner.Runner{}, err
	}

	validator := config.Validators[0]
	servers, err := validator.GetServers()
	if err != nil {
		return chaincmdrunner.Runner{}, err
	}

	nodeAddr, err := xurl.TCP(servers.RPC.Address)
	if err != nil {
		return chaincmdrunner.Runner{}, err
	}

	chainCommandOptions := []chaincmd.Option{
		chaincmd.WithChainID(id),
		chaincmd.WithHome(home),
		chaincmd.WithVersion(c.Version),
		chaincmd.WithNodeAddress(nodeAddr),
		chaincmd.WithKeyringBackend(backend),
	}

	cc := chaincmd.New(binary, chainCommandOptions...)

	ccrOptions := []chaincmdrunner.Option{}

	// Enable command output only when CLI verbosity is enabled
	if c.logOutputer != nil && c.logOutputer.Verbosity() == uilog.VerbosityVerbose {
		out := c.logOutputer.NewOutput(c.app.D(), 96)
		ccrOptions = append(
			ccrOptions,
			chaincmdrunner.Stdout(out.Stdout()),
			chaincmdrunner.Stderr(out.Stderr()),
		)
	}

	return chaincmdrunner.New(ctx, cc, ccrOptions...)
}
