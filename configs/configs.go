package configs

import (
	"log"
	"time"

	"github.com/caarlos0/env/v6"
	"github.com/joho/godotenv"
	"github.com/onflow/flow-go-sdk"
)

type Config struct {
	// -- Feature flags --

	DisableRawTransactions   bool `env:"FLOW_WALLET_DISABLE_RAWTX"`
	DisableFungibleTokens    bool `env:"FLOW_WALLET_DISABLE_FT"`
	DisableNonFungibleTokens bool `env:"FLOW_WALLET_DISABLE_NFT"`
	DisableChainEvents       bool `env:"FLOW_WALLET_DISABLE_CHAIN_EVENTS"`

	// -- Admin account --

	AdminAddress    string `env:"FLOW_WALLET_ADMIN_ADDRESS,notEmpty"`
	AdminKeyIndex   int    `env:"FLOW_WALLET_ADMIN_KEY_INDEX" envDefault:"0"`
	AdminKeyType    string `env:"FLOW_WALLET_ADMIN_KEY_TYPE" envDefault:"local"`
	AdminPrivateKey string `env:"FLOW_WALLET_ADMIN_PRIVATE_KEY,notEmpty"`
	// This sets the number of proposal keys to be used on the admin account.
	// You can increase transaction throughput by using multiple proposal keys for
	// parallel transaction execution.
	AdminProposalKeyCount uint16 `env:"FLOW_WALLET_ADMIN_PROPOSAL_KEY_COUNT" envDefault:"1"`

	// -- Keys --

	// When "DefaultKeyType" is set to "local", private keys are generated by the API
	// and stored as encrypted text in the database.
	// KMS key types:
	// - aws_kms
	// - google_kms
	DefaultKeyType  string `env:"FLOW_WALLET_DEFAULT_KEY_TYPE" envDefault:"local"`
	DefaultKeyIndex int    `env:"FLOW_WALLET_DEFAULT_KEY_INDEX" envDefault:"0"`
	// If the default of "-1" is used for "DefaultKeyWeight"
	// the service will use flow.AccountKeyWeightThreshold from the Flow SDK.
	DefaultKeyWeight int    `env:"FLOW_WALLET_DEFAULT_KEY_WEIGHT" envDefault:"-1"`
	DefaultSignAlgo  string `env:"FLOW_WALLET_DEFAULT_SIGN_ALGO" envDefault:"ECDSA_P256"`
	DefaultHashAlgo  string `env:"FLOW_WALLET_DEFAULT_HASH_ALGO" envDefault:"SHA3_256"`
	// This symmetrical key is used to encrypt private keys
	// that are stored in the database. Values per type:
	// - local: 32 bytes long encryption key
	// - aws_kms: key ARN, e.g. arn:aws:kms:us-west-1:123456789000:key/00000000-1111-2222-3333-444444444444
	// - google_kms: key resource name (without version info), e.g. projects/my-project/locations/europe-north1/keyRings/my-keyring/cryptoKeys/my-encryption-key
	EncryptionKey string `env:"FLOW_WALLET_ENCRYPTION_KEY,notEmpty"`
	// Encryption key type, one of: local, aws_kms, google_kms
	EncryptionKeyType string `env:"FLOW_WALLET_ENCRYPTION_KEY_TYPE,notEmpty" envDefault:"local"`

	// -- Database --

	DatabaseDSN     string `env:"FLOW_WALLET_DATABASE_DSN" envDefault:"wallet.db"`
	DatabaseType    string `env:"FLOW_WALLET_DATABASE_TYPE" envDefault:"sqlite"`
	DatabaseVersion string `env:"FLOW_WALLET_DATABASE_VERSION" envDefault:""`

	// -- Host and chain access --

	Host          string       `env:"FLOW_WALLET_HOST"`
	Port          int          `env:"FLOW_WALLET_PORT" envDefault:"3000"`
	AccessAPIHost string       `env:"FLOW_WALLET_ACCESS_API_HOST,notEmpty"`
	ChainID       flow.ChainID `env:"FLOW_WALLET_CHAIN_ID" envDefault:"flow-emulator"`

	// -- Templates --

	EnabledTokens []string `env:"FLOW_WALLET_ENABLED_TOKENS" envSeparator:","`

	// -- Workerpool --

	// Defines the maximum number of active jobs that can be queued before
	// new jobs are rejected.
	WorkerQueueCapacity uint `env:"FLOW_WALLET_WORKER_QUEUE_CAPACITY" envDefault:"1000"`
	// Number of concurrent workers handling incoming jobs.
	// You can increase the number of workers if you're sending
	// too many transactions and find that the queue is often backlogged.
	WorkerCount uint `env:"FLOW_WALLET_WORKER_COUNT" envDefault:"100"`

	// -- Google KMS --

	GoogleKMSProjectID  string `env:"FLOW_WALLET_GOOGLE_KMS_PROJECT_ID"`
	GoogleKMSLocationID string `env:"FLOW_WALLET_GOOGLE_KMS_LOCATION_ID"`
	GoogleKMSKeyRingID  string `env:"FLOW_WALLET_GOOGLE_KMS_KEYRING_ID"`

	// -- Misc --

	// Duration for which to wait for a transaction seal, if 0 wait indefinitely.
	// Valid time units are "ns", "us" (or "µs"), "ms", "s", "m", "h".
	// For more info: https://pkg.go.dev/time#ParseDuration
	TransactionTimeout time.Duration `env:"FLOW_WALLET_TRANSACTION_TIMEOUT" envDefault:"0"`

	// Set the starting height for event polling. This won't have any effect if the value in
	// database (chain_event_status[0].latest_height) is greater.
	// If 0 (default) use latest block height if starting fresh (no previous value in database).
	ChainListenerStartingHeight uint64 `env:"FLOW_WALLET_EVENTS_STARTING_HEIGHT" envDefault:"0"`
	// Maximum number of blocks to check at once.
	ChainListenerMaxBlocks uint64 `env:"FLOW_WALLET_EVENTS_MAX_BLOCKS" envDefault:"100"`
	// Valid time units are "ns", "us" (or "µs"), "ms", "s", "m", "h".
	// For more info: https://pkg.go.dev/time#ParseDuration
	ChainListenerInterval time.Duration `env:"FLOW_WALLET_EVENTS_INTERVAL" envDefault:"10s"`
}

type Options struct {
	EnvFilePath string
}

// ParseConfig parses environment variables and flags to a valid Config.
func ParseConfig(opt *Options) (*Config, error) {
	if opt != nil && opt.EnvFilePath != "" {
		// Load variables from a file to the environment of the process
		if err := godotenv.Load(opt.EnvFilePath); err != nil {
			log.Printf("Could not load environment variables from file.\n%s\nIf running inside a docker container this can be ignored.\n\n", err)
		}
	}

	cfg := Config{}
	if err := env.Parse(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}
