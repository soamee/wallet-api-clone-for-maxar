package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/caarlos0/env/v6"
	"github.com/eqlabs/flow-wallet-service/accounts"
	"github.com/eqlabs/flow-wallet-service/datastore/gorm"
	"github.com/eqlabs/flow-wallet-service/flow_helpers"
	"github.com/eqlabs/flow-wallet-service/handlers"
	"github.com/eqlabs/flow-wallet-service/jobs"
	"github.com/eqlabs/flow-wallet-service/keys"
	"github.com/eqlabs/flow-wallet-service/keys/basic"
	"github.com/eqlabs/flow-wallet-service/templates"
	"github.com/eqlabs/flow-wallet-service/tokens"
	"github.com/eqlabs/flow-wallet-service/transactions"
	"github.com/gorilla/mux"
	"github.com/joho/godotenv"
	"github.com/onflow/flow-go-sdk"
	"github.com/onflow/flow-go-sdk/client"
	"go.uber.org/goleak"
	"google.golang.org/grpc"
)

const (
	testDbDSN  = "test.db"
	testDbType = "sqlite"
)

var (
	cfg    testConfig
	logger *log.Logger
)

type testConfig struct {
	AccessApiHost string       `env:"ACCESS_API_HOST,required"`
	AdminAddress  string       `env:"ADMIN_ADDRESS,required"`
	ChainId       flow.ChainID `env:"CHAIN_ID" envDefault:"flow-emulator"`
}

type TestLogger struct {
	t *testing.T
}

func (tl *TestLogger) Write(p []byte) (n int, err error) {
	tl.t.Log(fmt.Sprintf("%s", p))
	return len(p), nil
}

type httpTestStep struct {
	name        string
	method      string
	body        io.Reader
	contentType string
	url         string
	expected    string
	status      int
	sync        bool
}

func handleStepRequest(s httpTestStep, r *mux.Router, t *testing.T) *httptest.ResponseRecorder {
	req, err := http.NewRequest(s.method, s.url, s.body)
	if err != nil {
		t.Fatal(err)
	}

	if s.contentType != "" {
		req.Header.Set("content-type", "application/json")
	}

	if s.sync {
		req.Header.Set(handlers.SyncHeader, "go ahead")
	}

	rr := httptest.NewRecorder()

	r.ServeHTTP(rr, req)

	status := rr.Code
	// Check the status code is what we expect.
	if status != s.status {
		t.Errorf("handler returned wrong status code: got %v want %v",
			status, s.status)
	}

	// Check the response body is what we expect.
	re := regexp.MustCompile(s.expected)
	match := re.FindString(rr.Body.String())
	if match == "" {
		t.Errorf("handler returned unexpected body: got %s want %v", rr.Body.String(), re)
	}

	return rr
}

func TestMain(m *testing.M) {

	if err := godotenv.Load(".env.test"); err != nil {
		log.Println("WARNING: Could not load environment variables from file; ", err)
	}

	if err := os.Setenv("DATABASE_DSN", testDbDSN); err != nil {
		panic(err)
	}
	if err := os.Setenv("DATABASE_TYPE", testDbType); err != nil {
		panic(err)
	}

	if err := env.Parse(&cfg); err != nil {
		panic(err)
	}

	logger = log.New(io.Discard, "", log.LstdFlags)

	exitcode := m.Run()

	os.Exit(exitcode)
}

func TestAccountServices(t *testing.T) {
	ignoreOpenCensus := goleak.IgnoreTopFunction("go.opencensus.io/stats/view.(*worker).start")
	defer goleak.VerifyNone(t, ignoreOpenCensus)

	fc, err := client.New(cfg.AccessApiHost, grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	defer fc.Close()

	db, err := gorm.New()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(testDbDSN)
	defer gorm.Close(db)

	jobStore := jobs.NewGormStore(db)
	accountStore := accounts.NewGormStore(db)
	keyStore := keys.NewGormStore(db)

	templateStore := templates.NewGormStore(db)
	templateService := templates.NewService(templateStore)

	km := basic.NewKeyManager(keyStore, fc)

	wp := jobs.NewWorkerPool(nil, jobStore)
	defer wp.Stop()
	wp.AddWorker(1)

	service := accounts.NewService(accountStore, km, fc, wp, nil, templateService)

	t.Run("admin init", func(t *testing.T) {
		service.InitAdminAccount()
	})

	t.Run("sync create", func(t *testing.T) {
		_, account, err := service.Create(context.Background(), true)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := flow_helpers.ValidateAddress(account.Address, flow.Emulator); err != nil {
			t.Errorf("Account has an invalid address: '%s'", account.Address)
		}
	})

	t.Run("async create", func(t *testing.T) {
		job, _, err := service.Create(context.Background(), false)
		if err != nil {
			t.Fatal(err)
		}

		if job.Status != jobs.Accepted && job.Status != jobs.Complete {
			t.Errorf("expected job status to be %s or %s but got %s",
				jobs.Accepted, jobs.Complete, job.Status)
		}

		for job.Status == jobs.Accepted {
			time.Sleep(10 * time.Millisecond)
		}

		if job.Status != jobs.Complete {
			t.Errorf("expected job status to be %s got %s", jobs.Complete, job.Status)
		}

		account, err := service.Details(job.Result)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := flow_helpers.ValidateAddress(account.Address, flow.Emulator); err != nil {
			t.Errorf("Account has an invalid address: '%s'", account.Address)
		}

		if len(account.Keys) != 0 {
			t.Error("Account should not expose keys")
		}
	})

	t.Run("async create thrice", func(t *testing.T) {
		_, _, err1 := service.Create(context.Background(), false) // Goes immediately to processing
		_, _, err2 := service.Create(context.Background(), false) // Queues - queue now full
		_, _, err3 := service.Create(context.Background(), false) // Should not fit, sometimes does
		if err1 != nil {
			t.Error(err1)
		}
		if err2 != nil {
			t.Error(err2)
		}
		if err3 == nil {
			t.Log("expected 503 'max capacity reached, try again later' but got no error")
		}
	})
}

func TestAccountHandlers(t *testing.T) {
	ignoreOpenCensus := goleak.IgnoreTopFunction("go.opencensus.io/stats/view.(*worker).start")
	defer goleak.VerifyNone(t, ignoreOpenCensus)

	fc, err := client.New(cfg.AccessApiHost, grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	defer fc.Close()

	db, err := gorm.New()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(testDbDSN)
	defer gorm.Close(db)

	jobStore := jobs.NewGormStore(db)
	keyStore := keys.NewGormStore(db)

	templateStore := templates.NewGormStore(db)
	templateService := templates.NewService(templateStore)

	km := basic.NewKeyManager(keyStore, fc)

	wp := jobs.NewWorkerPool(nil, jobStore)
	defer wp.Stop()
	wp.AddWorker(1)

	store := accounts.NewGormStore(db)
	service := accounts.NewService(store, km, fc, wp, nil, templateService)
	h := handlers.NewAccounts(logger, service)

	router := mux.NewRouter()
	router.Handle("/", h.List()).Methods(http.MethodGet)
	router.Handle("/", h.Create()).Methods(http.MethodPost)
	router.Handle("/{address}", h.Details()).Methods(http.MethodGet)

	var tempAccAddress string

	// NOTE: The order of the test "steps" matters
	steps := []struct {
		name     string
		method   string
		url      string
		expected string
		status   int
	}{
		{
			name:     "list db empty",
			method:   http.MethodGet,
			url:      "/",
			expected: `(?m)^\[\]$`,
			status:   http.StatusOK,
		},
		{
			name:     "create",
			method:   http.MethodPost,
			url:      "/",
			expected: `(?m)^{"jobId":".+"}$`,
			status:   http.StatusCreated,
		},
		{
			name:     "list db not empty",
			method:   http.MethodGet,
			url:      "/",
			expected: `(?m)^\[{"address":".+".*}\]$`,
			status:   http.StatusOK,
		},
		{
			name:     "details invalid address",
			method:   http.MethodGet,
			url:      "/invalid-address",
			expected: "not a valid address",
			status:   http.StatusBadRequest,
		},
		{
			name:     "details unknown address",
			method:   http.MethodGet,
			url:      "/0f7025fa05b578e3",
			expected: "record not found",
			status:   http.StatusNotFound,
		},
		{
			name:     "details known address",
			method:   http.MethodGet,
			url:      "/<address>",
			expected: `(?m)^{"address":".+"}$`,
			status:   http.StatusOK,
		},
	}

	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			replacer := strings.NewReplacer(
				"<address>", tempAccAddress,
			)

			url := replacer.Replace(string(step.url))

			req, err := http.NewRequest(step.method, url, nil)
			if err != nil {
				t.Fatal(err)
			}

			req.Context()

			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			// Check the status code is what we expect.
			if status := rr.Code; status != step.status {
				t.Errorf("handler returned wrong status code: got %v want %v",
					status, step.status)
			}

			// If this step was creating a new account
			// wait for the account to become available
			// and store the new account in "tempAcc".
			if step.status == http.StatusCreated {
				jobService := jobs.NewService(jobStore)
				var rJob jobs.Job
				json.Unmarshal(rr.Body.Bytes(), &rJob)
				id := rJob.ID.String()
				job, _ := jobService.Details(id)
				for job.Status == jobs.Accepted {
					job, _ = jobService.Details(id)
				}
				tempAccAddress = job.Result
			}

			// Check the response body is what we expect.
			re := regexp.MustCompile(step.expected)
			match := re.FindString(rr.Body.String())
			if match == "" {
				t.Errorf("handler returned unexpected body: got %q want %v", rr.Body.String(), re)
			}
		})
	}
}

func TestTransactionHandlers(t *testing.T) {
	ignoreOpenCensus := goleak.IgnoreTopFunction("go.opencensus.io/stats/view.(*worker).start")
	defer goleak.VerifyNone(t, ignoreOpenCensus)

	// logger = log.New(&TestLogger{t}, "", log.Lshortfile)

	fc, err := client.New(cfg.AccessApiHost, grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	defer fc.Close()

	db, err := gorm.New()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(testDbDSN)
	defer gorm.Close(db)

	jobStore := jobs.NewGormStore(db)
	keyStore := keys.NewGormStore(db)

	templateStore := templates.NewGormStore(db)
	templateService := templates.NewService(templateStore)

	km := basic.NewKeyManager(keyStore, fc)

	wp := jobs.NewWorkerPool(nil, jobStore)
	defer wp.Stop()
	wp.AddWorker(1)

	store := transactions.NewGormStore(db)
	service := transactions.NewService(store, km, fc, wp)
	h := handlers.NewTransactions(logger, service)

	router := mux.NewRouter()
	router.Handle("/{address}/transactions", h.List()).Methods(http.MethodGet)
	router.Handle("/{address}/transactions", h.Create()).Methods(http.MethodPost)
	router.Handle("/{address}/transactions/{transactionId}", h.Details()).Methods(http.MethodGet)

	token, err := templateService.GetTokenByName("FlowToken")
	if err != nil {
		t.Fatal(err)
	}

	tFlow := templates.FungibleTransferCode(token)
	tFlowBytes, err := json.Marshal(tFlow)
	if err != nil {
		t.Fatal(err)
	}

	validTransferFlow := fmt.Sprintf(`{
		"code":%s,
		"arguments":[{"type":"UFix64","value":"1.0"},{"type":"Address","value":"%s"}]
	}`, tFlowBytes, cfg.AdminAddress)

	validHello := `{
		"code":"transaction(greeting: String) { execute { log(greeting.concat(\", World!\")) }}",
		"arguments":[{"type":"String","value":"Hello"}]
	}`

	invalidHello := `{
		"code":"this is not valid cadence code",
		"arguments":[{"type":"String","value":"Hello"}]
	}`

	var tempTxId string

	// NOTE: The order of the test "steps" matters
	steps := []struct {
		name        string
		method      string
		body        io.Reader
		contentType string
		url         string
		expected    string
		status      int
		sync        bool
	}{
		{
			name:     "list db empty",
			method:   http.MethodGet,
			url:      fmt.Sprintf("/%s/transactions", cfg.AdminAddress),
			expected: `(?m)^\[\]$`,
			status:   http.StatusOK,
		},
		{
			name:     "list db empty invalid address",
			method:   http.MethodGet,
			url:      "/invalid-address/transactions",
			expected: "not a valid address",
			status:   http.StatusBadRequest,
		},
		{
			name:        "create ok async",
			method:      http.MethodPost,
			contentType: "application/json",
			body:        strings.NewReader(validHello),
			url:         fmt.Sprintf("/%s/transactions", cfg.AdminAddress),
			expected:    `(?m)^{"jobId":".+"}$`,
			status:      http.StatusCreated,
		},
		{
			name:        "create ok sync",
			method:      http.MethodPost,
			contentType: "application/json",
			body:        strings.NewReader(validHello),
			url:         fmt.Sprintf("/%s/transactions", cfg.AdminAddress),
			expected:    `(?m)^{"transactionId":".+"}$`,
			status:      http.StatusCreated,
			sync:        true,
		},
		{
			name:        "create invalid content-type",
			method:      http.MethodPost,
			contentType: "",
			body:        strings.NewReader(validHello),
			url:         fmt.Sprintf("/%s/transactions", cfg.AdminAddress),
			expected:    "Unsupported content type",
			status:      http.StatusUnsupportedMediaType,
			sync:        true,
		},
		{
			name:        "create ok sync requires authorizer",
			method:      http.MethodPost,
			contentType: "application/json",
			body:        strings.NewReader(validTransferFlow),
			url:         fmt.Sprintf("/%s/transactions", cfg.AdminAddress),
			expected:    `(?m)^{"transactionId":".+"}$`,
			status:      http.StatusCreated,
			sync:        true,
		},
		{
			name:        "create nil body sync",
			method:      http.MethodPost,
			contentType: "application/json",
			body:        nil,
			url:         fmt.Sprintf("/%s/transactions", cfg.AdminAddress),
			expected:    "empty body",
			status:      http.StatusBadRequest,
			sync:        true,
		},
		{
			name:        "create empty body sync",
			method:      http.MethodPost,
			contentType: "application/json",
			body:        strings.NewReader(""),
			url:         fmt.Sprintf("/%s/transactions", cfg.AdminAddress),
			expected:    "empty body",
			status:      http.StatusBadRequest,
			sync:        true,
		},
		{
			name:        "create invalid body sync",
			method:      http.MethodPost,
			contentType: "application/json",
			body:        strings.NewReader("notvalidobject"),
			url:         fmt.Sprintf("/%s/transactions", cfg.AdminAddress),
			expected:    "invalid body",
			status:      http.StatusBadRequest,
			sync:        true,
		},
		{
			name:        "create invalid code sync",
			method:      http.MethodPost,
			contentType: "application/json",
			body:        strings.NewReader(invalidHello),
			url:         fmt.Sprintf("/%s/transactions", cfg.AdminAddress),
			expected:    "Parsing failed",
			status:      http.StatusBadRequest,
			sync:        true,
		},
		{
			name:        "create invalid address sync",
			method:      http.MethodPost,
			contentType: "application/json",
			body:        strings.NewReader(validHello),
			url:         "/invalid-address/transactions",
			expected:    "not a valid address",
			status:      http.StatusBadRequest,
			sync:        true,
		},
		{
			name:     "list db not empty",
			method:   http.MethodGet,
			url:      fmt.Sprintf("/%s/transactions", cfg.AdminAddress),
			expected: `(?m)^\[{"transactionId":".+".*}\]$`,
			status:   http.StatusOK,
		},
		{
			name:     "details invalid id",
			method:   http.MethodGet,
			url:      fmt.Sprintf("/%s/transactions/invalid-id", cfg.AdminAddress),
			expected: "not a valid transaction id",
			status:   http.StatusBadRequest,
		},
		{
			name:     "details unknown id",
			method:   http.MethodGet,
			url:      fmt.Sprintf("/%s/transactions/0e4f500d6965c7fc0ff1239525e09eb9dd27c00a511976e353d9f6a44ca22921", cfg.AdminAddress),
			expected: "transaction not found",
			status:   http.StatusNotFound,
		},
		{
			name:     "details known id",
			method:   http.MethodGet,
			url:      fmt.Sprintf("/%s/transactions/<id>", cfg.AdminAddress),
			expected: `(?m)^{"transactionId":".+"}$`,
			status:   http.StatusOK,
		},
		{
			name:     "details invalid address",
			method:   http.MethodGet,
			url:      "/invalid-address/transactions/invalid-id",
			expected: "not a valid address",
			status:   http.StatusBadRequest,
		},
	}

	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			replacer := strings.NewReplacer(
				"<id>", tempTxId,
			)

			url := replacer.Replace(string(step.url))

			req, err := http.NewRequest(step.method, url, step.body)
			if err != nil {
				t.Fatal(err)
			}

			if step.contentType != "" {
				req.Header.Set("content-type", "application/json")
			}

			if step.sync {
				req.Header.Set(handlers.SyncHeader, "go ahead")
			}

			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			status := rr.Code
			// Check the status code is what we expect.
			if status != step.status {
				t.Errorf("handler returned wrong status code: got %v want %v",
					status, step.status)
			}

			// If this step was creating a new transaction store the new txId in "tempTxId".
			if step.sync && status == http.StatusCreated {
				var transaction transactions.Transaction
				json.Unmarshal(rr.Body.Bytes(), &transaction)
				tempTxId = transaction.TransactionId
			}

			// Check the response body is what we expect.
			re := regexp.MustCompile(step.expected)
			match := re.FindString(rr.Body.String())
			if match == "" {
				t.Errorf("handler returned unexpected body: got %q want %v", rr.Body.String(), re)
			}
		})
	}
}

func TestScriptsHandlers(t *testing.T) {
	ignoreOpenCensus := goleak.IgnoreTopFunction("go.opencensus.io/stats/view.(*worker).start")
	defer goleak.VerifyNone(t, ignoreOpenCensus)

	fc, err := client.New(cfg.AccessApiHost, grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	defer fc.Close()

	service := transactions.NewService(nil, nil, fc, nil)
	h := handlers.NewTransactions(logger, service)

	router := mux.NewRouter()
	router.Handle("/", h.ExecuteScript()).Methods(http.MethodPost)

	steps := []struct {
		name        string
		method      string
		body        io.Reader
		contentType string
		expected    string
		status      int
	}{
		{
			name:   "int 1",
			method: http.MethodPost,
			body: strings.NewReader(`{
				"code":"pub fun main(): Int { return 1 }",
				"arguments":[]
			}`),
			contentType: "application/json",
			expected:    `(?m)^{"Value":1}$`,
			status:      http.StatusOK,
		},
		{
			name:   "get supply",
			method: http.MethodPost,
			body: strings.NewReader(`{
				"code":"import FlowToken from 0x0ae53cb6e3f42a79\npub fun main(): UFix64 {\nlet supply = FlowToken.totalSupply\nreturn supply\n}",
				"arguments":[]
			}`),
			contentType: "application/json",
			expected:    "1000000000000000000",
			status:      http.StatusOK,
		},
		{
			name:   "get balance",
			method: http.MethodPost,
			body: strings.NewReader(fmt.Sprintf(`{
				"code":"import FungibleToken from 0xee82856bf20e2aa6\nimport FlowToken from 0x0ae53cb6e3f42a79\npub fun main(account: Address): UFix64 {\nlet vaultRef = getAccount(account)\n.getCapability(/public/flowTokenBalance)\n.borrow<&FlowToken.Vault{FungibleToken.Balance}>()\n?? panic(\"Could not borrow Balance reference to the Vault\")\nreturn vaultRef.balance\n}",
				"arguments":[{"type":"Address","value":"%s"}]
			}`, cfg.AdminAddress)),
			contentType: "application/json",
			expected:    "\\d+",
			status:      http.StatusOK,
		},
	}

	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			req, err := http.NewRequest(step.method, "/", step.body)
			if err != nil {
				t.Fatal(err)
			}

			if step.contentType != "" {
				req.Header.Set("content-type", "application/json")
			}

			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			status := rr.Code
			// Check the status code is what we expect.
			if status != step.status {
				t.Errorf("handler returned wrong status code: got %v want %v",
					status, step.status)
			}

			// Check the response body is what we expect.
			re := regexp.MustCompile(step.expected)
			match := re.FindString(rr.Body.String())
			if match == "" {
				t.Errorf("handler returned unexpected body: got %s want %v", rr.Body.String(), re)
			}
		})
	}
}

func TestTokenServices(t *testing.T) {
	ignoreOpenCensus := goleak.IgnoreTopFunction("go.opencensus.io/stats/view.(*worker).start")
	defer goleak.VerifyNone(t, ignoreOpenCensus)

	fc, err := client.New(cfg.AccessApiHost, grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	defer fc.Close()

	db, err := gorm.New()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(testDbDSN)
	defer gorm.Close(db)

	jobStore := jobs.NewGormStore(db)
	accountStore := accounts.NewGormStore(db)
	keyStore := keys.NewGormStore(db)
	transactionStore := transactions.NewGormStore(db)
	tokenStore := tokens.NewGormStore(db)

	templateStore := templates.NewGormStore(db)
	templateService := templates.NewService(templateStore)

	km := basic.NewKeyManager(keyStore, fc)

	wp := jobs.NewWorkerPool(nil, jobStore)
	defer wp.Stop()
	wp.AddWorker(1)

	transactionService := transactions.NewService(transactionStore, km, fc, wp)
	accountService := accounts.NewService(accountStore, km, fc, wp, transactionService, templateService)
	service := tokens.NewService(tokenStore, km, fc, transactionService, templateService)

	t.Run("account can make a transaction", func(t *testing.T) {
		// Create an account
		_, account, err := accountService.Create(context.Background(), true)
		if err != nil {
			t.Fatal(err)
		}

		// Fund the account from service account
		_, _, err = service.CreateFtWithdrawal(
			context.Background(),
			true,
			"FlowToken",
			cfg.AdminAddress,
			account.Address,
			"1.0",
		)

		if err != nil {
			t.Fatal(err)
		}

		_, transfer, err := service.CreateFtWithdrawal(
			context.Background(),
			true,
			"FlowToken",
			account.Address,
			cfg.AdminAddress,
			"1.0",
		)

		if err != nil {
			t.Fatal(err)
		}

		if flow.HexToID(transfer.TransactionId) == flow.EmptyID {
			t.Fatalf("Expected TransactionId not to be empty")
		}
	})

	t.Run("account can not make a transaction without funds", func(t *testing.T) {
		// Create an account
		_, account, err := accountService.Create(context.Background(), true)
		if err != nil {
			t.Fatal(err)
		}

		_, tx, err := service.CreateFtWithdrawal(
			context.Background(),
			true,
			"FlowToken",
			account.Address,
			cfg.AdminAddress,
			"1.0",
		)

		if tx == nil {
			t.Fatal("Expected transaction not to be nil")
		}

		if flow.HexToID(tx.TransactionId) == flow.EmptyID {
			t.Fatal("Expected TransactionId not to be empty")
		}

		if err == nil {
			t.Fatal("Expected an error")
		}
	})

	t.Run("manage fusd for an account", func(t *testing.T) {
		tokenName := "FUSD"

		ctx := context.Background()

		// Make sure FUSD is deployed
		_, _, err := service.DeployTokenContractForAccount(ctx, true, tokenName, cfg.AdminAddress)
		if err != nil {
			if !strings.Contains(err.Error(), "cannot overwrite existing contract") {
				t.Fatal(err)
			}
		}

		// Setup the admin account to be able to handle FUSD
		_, _, err = accountService.SetupToken(ctx, true, tokenName, cfg.AdminAddress)
		if err != nil {
			if !strings.Contains(err.Error(), "vault exists") {
				t.Fatal(err)
			}
		}

		// Create an account
		_, account, err := accountService.Create(ctx, true)
		if err != nil {
			t.Fatal(err)
		}

		// Setup the new account to be able to handle FUSD
		_, setupTx, err := accountService.SetupToken(ctx, true, tokenName, account.Address)
		if err != nil {
			t.Fatal(err)
		}

		if setupTx.TransactionType != transactions.FtSetup {
			t.Errorf("expected %s but got %s", transactions.FtSetup, setupTx.TransactionType)
		}

		// Create a withdrawal, should error as we can not mint FUSD right now
		_, _, err = service.CreateFtWithdrawal(ctx, true, tokenName, cfg.AdminAddress, account.Address, "1.0")
		if err != nil {
			if !strings.Contains(err.Error(), "Amount withdrawn must be less than or equal than the balance of the Vault") {
				t.Fatal(err)
			}
		}

	})

	t.Run(("try to setup a non-existent token"), func(t *testing.T) {
		tokenName := "some-non-existent-token"

		ctx := context.Background()

		// Create an account
		_, account, err := accountService.Create(ctx, true)
		if err != nil {
			t.Fatal(err)
		}

		// Setup the new account to be able to handle the non-existent token
		_, _, err = accountService.SetupToken(ctx, true, tokenName, account.Address)
		if err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestTokenHandlers(t *testing.T) {
	ignoreOpenCensus := goleak.IgnoreTopFunction("go.opencensus.io/stats/view.(*worker).start")
	defer goleak.VerifyNone(t, ignoreOpenCensus)

	fc, err := client.New(cfg.AccessApiHost, grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	defer fc.Close()

	db, err := gorm.New()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(testDbDSN)
	defer gorm.Close(db)

	jobStore := jobs.NewGormStore(db)
	accountStore := accounts.NewGormStore(db)
	keyStore := keys.NewGormStore(db)
	transactionStore := transactions.NewGormStore(db)
	tokenStore := tokens.NewGormStore(db)

	templateStore := templates.NewGormStore(db)
	templateService := templates.NewService(templateStore)

	km := basic.NewKeyManager(keyStore, fc)

	wp := jobs.NewWorkerPool(nil, jobStore)
	defer wp.Stop()
	wp.AddWorker(1)

	transactionService := transactions.NewService(transactionStore, km, fc, wp)
	accountService := accounts.NewService(accountStore, km, fc, wp, transactionService, templateService)
	service := tokens.NewService(tokenStore, km, fc, transactionService, templateService)

	tokenHandlers := handlers.NewTokens(logger, service, templates.FT)
	accountHandlers := handlers.NewAccounts(logger, accountService)

	router := mux.NewRouter()
	router.Handle("/{address}/fungible-tokens", accountHandlers.AccountTokens(templates.FT)).Methods(http.MethodGet)
	router.Handle("/{address}/fungible-tokens/{tokenName}", accountHandlers.SetupToken()).Methods(http.MethodPost)
	router.Handle("/{address}/fungible-tokens/{tokenName}", tokenHandlers.Details()).Methods(http.MethodGet)
	router.Handle("/{address}/fungible-tokens/{tokenName}/withdrawals", tokenHandlers.CreateWithdrawal()).Methods(http.MethodPost)
	router.Handle("/{address}/fungible-tokens/{tokenName}/withdrawals", tokenHandlers.ListWithdrawals()).Methods(http.MethodGet)
	router.Handle("/{address}/fungible-tokens/{tokenName}/withdrawals/{transactionId}", tokenHandlers.GetWithdrawal()).Methods(http.MethodGet)
	router.Handle("/{address}/fungible-tokens/{tokenName}/deposits", tokenHandlers.ListDeposits()).Methods(http.MethodGet)
	router.Handle("/{address}/fungible-tokens/{tokenName}/deposits/{transactionId}", tokenHandlers.GetDeposit()).Methods(http.MethodGet)

	// Setup tokens

	// Make sure FUSD is deployed
	_, _, err = service.DeployTokenContractForAccount(context.Background(), true, "FUSD", cfg.AdminAddress)
	if err != nil {
		if !strings.Contains(err.Error(), "cannot overwrite existing contract") {
			t.Fatal(err)
		}
	}

	// Create a few accounts
	aa := make([]*accounts.Account, 4)
	for i := 0; i < 4; i++ {
		_, a, err := accountService.Create(context.Background(), true)
		if err != nil {
			t.Fatal(err)
		}
		aa[i] = a
	}

	setupTokenSteps := []httpTestStep{
		{
			name:        "Setup FUSD valid async",
			method:      http.MethodPost,
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens/%s", aa[0].Address, "FUSD"),
			expected:    `(?m)^{"jobId":".+".*}$`,
			status:      http.StatusCreated,
		},
		{
			name:        "Setup FUSD valid sync",
			sync:        true,
			method:      http.MethodPost,
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens/%s", aa[1].Address, "FUSD"),
			expected:    `(?m)^{"transactionId":".+".*}$`,
			status:      http.StatusCreated,
		},
	}

	for _, s := range setupTokenSteps {
		t.Run(s.name, func(t *testing.T) {
			handleStepRequest(s, router, t)
			time.Sleep(100 * time.Millisecond)
		})
	}

	// Token details
	detailtsSteps := []httpTestStep{
		{
			name:        "FlowToken details",
			method:      http.MethodGet,
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens/%s", aa[1].Address, "FlowToken"),
			expected:    `(?m)^{"name":"FlowToken","balance":".+"}$`,
			status:      http.StatusOK,
		},
		{
			name:        "FUSD details",
			method:      http.MethodGet,
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens/%s", aa[1].Address, "FUSD"),
			expected:    `(?m)^{"name":"FUSD\","balance":".+"}$`,
			status:      http.StatusOK,
		},
	}

	for _, s := range detailtsSteps {
		t.Run(s.name, func(t *testing.T) {
			handleStepRequest(s, router, t)
		})
	}

	// Token list
	listSteps := []httpTestStep{
		{
			name:        "list account fungible tokens",
			method:      http.MethodGet,
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens", aa[1].Address),
			expected:    `(?m)^\[{"name":"FUSD".*"name":"FlowToken".*}\]$`,
			status:      http.StatusOK,
		},
	}

	for _, s := range listSteps {
		t.Run(s.name, func(t *testing.T) {
			handleStepRequest(s, router, t)
		})
	}

	// Withdrawals
	token, err := templateService.GetTokenByName("FlowToken")
	if err != nil {
		t.Fatal(err)
	}

	_, account, err := accountService.Create(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}

	// Create withdrawals
	createWithdrawalSteps := []httpTestStep{
		{
			name:        "create withdrawal valid async",
			method:      http.MethodPost,
			body:        strings.NewReader(fmt.Sprintf(`{"recipient":"%s","amount":"1.0"}`, account.Address)),
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens/%s/withdrawals", cfg.AdminAddress, token.Name),
			expected:    `(?m)^{"jobId":".+"}$`,
			status:      http.StatusCreated,
		},
		{
			name:        "create withdrawal valid sync",
			sync:        true,
			method:      http.MethodPost,
			body:        strings.NewReader(fmt.Sprintf(`{"recipient":"%s","amount":"1.0"}`, account.Address)),
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens/%s/withdrawals", cfg.AdminAddress, token.Name),
			expected:    `(?m)^{"transactionId":".+"}$`,
			status:      http.StatusCreated,
		},
		{
			name:        "create withdrawal invalid recipient",
			method:      http.MethodPost,
			body:        strings.NewReader(`{"recipient":"","amount":"1.0"}`),
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens/%s/withdrawals", cfg.AdminAddress, token.Name),
			expected:    "not a valid address",
			status:      http.StatusBadRequest,
		},
		{
			name:        "create withdrawal invalid amount",
			method:      http.MethodPost,
			body:        strings.NewReader(fmt.Sprintf(`{"recipient":"%s","amount":""}`, account.Address)),
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens/%s/withdrawals", cfg.AdminAddress, token.Name),
			expected:    "missing decimal point",
			status:      http.StatusBadRequest,
		},
	}

	for _, s := range createWithdrawalSteps {
		t.Run(s.name, func(t *testing.T) {
			handleStepRequest(s, router, t)
		})
	}

	_, transfer, err := service.CreateFtWithdrawal(context.Background(), true, token.Name, cfg.AdminAddress, account.Address, "1.0")
	if err != nil {
		t.Fatal(err)
	}

	// Get withdrawals
	getWithdrawalSteps := []httpTestStep{
		{
			name:        "list withdrawals valid",
			method:      http.MethodGet,
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens/%s/withdrawals", cfg.AdminAddress, token.Name),
			expected:    `(?m)^\[{"transactionId":".+".*}\]$`,
			status:      http.StatusOK,
		},
		{
			name:        "list withdrawals empty",
			method:      http.MethodGet,
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens/%s/withdrawals", account.Address, token.Name),
			expected:    `(?m)^\[\]$`,
			status:      http.StatusOK,
		},
		{
			name:        "get withdrawal details valid",
			method:      http.MethodGet,
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens/%s/withdrawals/%s", cfg.AdminAddress, token.Name, transfer.TransactionId),
			expected:    fmt.Sprintf(`(?m)^{"transactionId":"%s".*}$`, transfer.TransactionId),
			status:      http.StatusOK,
		},
		{
			name:        "get withdrawal details invalid transaction id",
			method:      http.MethodGet,
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens/%s/withdrawals/invalid-transaction-id", cfg.AdminAddress, token.Name),
			expected:    "not a valid transaction id",
			status:      http.StatusBadRequest,
		},
		{
			name:        "get withdrawal details not found",
			method:      http.MethodGet,
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens/%s/withdrawals/%s", account.Address, token.Name, transfer.TransactionId),
			expected:    "record not found",
			status:      http.StatusNotFound,
		},
	}

	for _, s := range getWithdrawalSteps {
		t.Run(s.name, func(t *testing.T) {
			handleStepRequest(s, router, t)
		})
	}

	// List deposits
	listDepositSteps := []httpTestStep{
		{
			name:        "list deposits valid",
			method:      http.MethodGet,
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens/%s/deposits", account.Address, token.Name),
			expected:    `(?m)^\[{"transactionId":".+".*}\]$`,
			status:      http.StatusOK,
		},
		{
			name:        "list deposits invalid token name",
			method:      http.MethodGet,
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens/%s/deposits", account.Address, "some-invalid-token-name"),
			expected:    `record not found`,
			status:      http.StatusNotFound,
		},
		{
			name:        "list deposits invalid address",
			method:      http.MethodGet,
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens/%s/deposits", "0x1", token.Name),
			expected:    `not a valid address`,
			status:      http.StatusBadRequest,
		},
	}

	for _, s := range listDepositSteps {
		t.Run(s.name, func(t *testing.T) {
			handleStepRequest(s, router, t)
		})
	}

	// Get deposits
	getDepositSteps := []httpTestStep{
		{
			name:        "get deposit details valid",
			method:      http.MethodGet,
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens/%s/deposits/%s", account.Address, token.Name, transfer.TransactionId),
			expected:    `(?m)^{"transactionId":".+".*}$`,
			status:      http.StatusOK,
		},
		{
			name:        "get deposit details invalid token name",
			method:      http.MethodGet,
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens/%s/deposits/%s", account.Address, "some-invalid-token-name", transfer.TransactionId),
			expected:    `record not found`,
			status:      http.StatusNotFound,
		},
		{
			name:        "get deposit details invalid address",
			method:      http.MethodGet,
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens/%s/deposits/%s", "0x1", token.Name, transfer.TransactionId),
			expected:    `not a valid address`,
			status:      http.StatusBadRequest,
		},
		{
			name:        "get deposit details invalid transactionId",
			method:      http.MethodGet,
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens/%s/deposits/%s", account.Address, token.Name, "0"),
			expected:    `not a valid transaction id`,
			status:      http.StatusBadRequest,
		},
		{
			name:        "get deposit details 404",
			method:      http.MethodGet,
			contentType: "application/json",
			url:         fmt.Sprintf("/%s/fungible-tokens/%s/deposits/%s", aa[0].Address, token.Name, transfer.TransactionId),
			expected:    `record not found`,
			status:      http.StatusNotFound,
		},
	}

	for _, s := range getDepositSteps {
		t.Run(s.name, func(t *testing.T) {
			handleStepRequest(s, router, t)
		})
	}
}

func TestNFTDeployment(t *testing.T) {
	t.Skip("currently not supported")

	ignoreOpenCensus := goleak.IgnoreTopFunction("go.opencensus.io/stats/view.(*worker).start")
	defer goleak.VerifyNone(t, ignoreOpenCensus)

	fc, err := client.New(cfg.AccessApiHost, grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	defer fc.Close()

	db, err := gorm.New()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(testDbDSN)
	defer gorm.Close(db)

	templateStore := templates.NewGormStore(db)
	templateService := templates.NewService(templateStore)

	jobStore := jobs.NewGormStore(db)
	keyStore := keys.NewGormStore(db)
	transactionStore := transactions.NewGormStore(db)
	tokenStore := tokens.NewGormStore(db)

	km := basic.NewKeyManager(keyStore, fc)

	wp := jobs.NewWorkerPool(nil, jobStore)
	defer wp.Stop()
	wp.AddWorker(1)

	transactionService := transactions.NewService(transactionStore, km, fc, wp)
	tokenService := tokens.NewService(tokenStore, km, fc, transactionService, templateService)

	_, _, err = tokenService.DeployTokenContractForAccount(context.Background(), true, "ExampleNFT", cfg.AdminAddress)
	if err != nil {
		if !strings.Contains(err.Error(), "cannot overwrite existing contract") {
			t.Fatal(err)
		}
	}
}

func TestTemplateHandlers(t *testing.T) {
	ignoreOpenCensus := goleak.IgnoreTopFunction("go.opencensus.io/stats/view.(*worker).start")
	defer goleak.VerifyNone(t, ignoreOpenCensus)

	db, err := gorm.New()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(testDbDSN)
	defer gorm.Close(db)

	templateStore := templates.NewGormStore(db)
	templateService := templates.NewService(templateStore)
	templateHandler := handlers.NewTemplates(logger, templateService)

	router := mux.NewRouter()
	router.Handle("/tokens", templateHandler.AddToken()).Methods(http.MethodPost)
	router.Handle("/tokens", templateHandler.ListTokens(nil)).Methods(http.MethodGet)
	router.Handle("/tokens/{id_or_name}", templateHandler.GetToken()).Methods(http.MethodGet)
	router.Handle("/tokens/{id}", templateHandler.RemoveToken()).Methods(http.MethodDelete)

	addStepts := []httpTestStep{
		{
			name:        "Add with empty body",
			method:      http.MethodPost,
			contentType: "application/json",
			url:         "/tokens",
			expected:    `empty body`,
			status:      http.StatusBadRequest,
		},
		{
			name:        "Add with invalid body",
			method:      http.MethodPost,
			body:        strings.NewReader(`invalid`),
			contentType: "application/json",
			url:         "/tokens",
			expected:    `invalid body`,
			status:      http.StatusBadRequest,
		},
		{
			name:        "Add with invalid name",
			method:      http.MethodPost,
			body:        strings.NewReader(fmt.Sprintf(`{"name":"","address":"%s"}`, cfg.AdminAddress)),
			contentType: "application/json",
			url:         "/tokens",
			expected:    `not a valid name: ""`,
			status:      http.StatusBadRequest,
		},
		{
			name:        "Add with invalid address",
			method:      http.MethodPost,
			body:        strings.NewReader(`{"name":"TestToken","address":"0x1"}`),
			contentType: "application/json",
			url:         "/tokens",
			expected:    `not a valid address: "0x1"`,
			status:      http.StatusBadRequest,
		},
		{
			name:        "Add with valid body",
			method:      http.MethodPost,
			body:        strings.NewReader(fmt.Sprintf(`{"name":"TestToken","address":"%s"}`, cfg.AdminAddress)),
			contentType: "application/json",
			url:         "/tokens",
			expected:    fmt.Sprintf(`{"id":\d+,"name":"TestToken","address":"%s","type":"Unknown"}`, cfg.AdminAddress),
			status:      http.StatusCreated,
		},
		{
			name:        "Add duplicate",
			method:      http.MethodPost,
			body:        strings.NewReader(fmt.Sprintf(`{"name":"TestToken","address":"%s"}`, cfg.AdminAddress)),
			contentType: "application/json",
			url:         "/tokens",
			expected:    `UNIQUE constraint failed: tokens.name`,
			status:      http.StatusBadRequest,
		},
		{
			name:        "List not empty",
			method:      http.MethodGet,
			contentType: "application/json",
			url:         "/tokens",
			expected:    fmt.Sprintf(`\[*{"id":\d+,"name":"TestToken","address":"%s","type":"Unknown"}*\]`, cfg.AdminAddress),
			status:      http.StatusOK,
		},
	}

	getSteps := []httpTestStep{
		{
			name:        "Get invalid id",
			method:      http.MethodGet,
			contentType: "application/json",
			url:         "/tokens/invalid-id",
			expected:    `record not found`,
			status:      http.StatusNotFound,
		},
		{
			name:        "Get not found",
			method:      http.MethodGet,
			contentType: "application/json",
			url:         "/tokens/100",
			expected:    `record not found`,
			status:      http.StatusNotFound,
		},
		{
			name:        "Get valid id",
			method:      http.MethodGet,
			contentType: "application/json",
			url:         "/tokens/1",
			expected:    `{"id":1,.*"type":"FT"}`,
			status:      http.StatusOK,
		},
	}

	removeSteps := []httpTestStep{
		{
			name:        "Remove invalid id",
			method:      http.MethodDelete,
			contentType: "application/json",
			url:         "/tokens/invalid-id",
			expected:    `parsing "invalid-id": invalid syntax`,
			status:      http.StatusBadRequest,
		},
		{
			// Gorm won't return an error if deleting a non-existent entry
			name:        "Remove not found",
			method:      http.MethodDelete,
			contentType: "application/json",
			url:         "/tokens/100",
			expected:    `100`,
			status:      http.StatusOK,
		},
		{
			name:        "Remove valid id",
			method:      http.MethodDelete,
			contentType: "application/json",
			url:         "/tokens/1",
			expected:    "1",
			status:      http.StatusOK,
		},
	}

	typeSteps := []httpTestStep{
		{
			name:        "Add invalid type",
			method:      http.MethodPost,
			body:        strings.NewReader(fmt.Sprintf(`{"name":"TestToken2","address":"%s","type":"not-a-valid-type"}`, cfg.AdminAddress)),
			contentType: "application/json",
			url:         "/tokens",
			expected:    fmt.Sprintf(`{"id":\d+,"name":"TestToken2","address":"%s","type":"Unknown"}`, cfg.AdminAddress),
			status:      http.StatusCreated,
		},
		{
			name:        "Add FT valid",
			method:      http.MethodPost,
			body:        strings.NewReader(fmt.Sprintf(`{"name":"TestToken3","address":"%s","type":"FT"}`, cfg.AdminAddress)),
			contentType: "application/json",
			url:         "/tokens",
			expected:    fmt.Sprintf(`{"id":\d+,"name":"TestToken3","address":"%s","type":"FT"}`, cfg.AdminAddress),
			status:      http.StatusCreated,
		},
		{
			name:        "Add NFT valid",
			method:      http.MethodPost,
			body:        strings.NewReader(fmt.Sprintf(`{"name":"TestToken4","address":"%s","type":"NFT"}`, cfg.AdminAddress)),
			contentType: "application/json",
			url:         "/tokens",
			expected:    fmt.Sprintf(`{"id":\d+,"name":"TestToken4","address":"%s","type":"NFT"}`, cfg.AdminAddress),
			status:      http.StatusCreated,
		},
	}

	for _, s := range addStepts {
		t.Run(s.name, func(t *testing.T) {
			handleStepRequest(s, router, t)
		})
	}

	for _, s := range getSteps {
		t.Run(s.name, func(t *testing.T) {
			handleStepRequest(s, router, t)
		})
	}

	for _, s := range removeSteps {
		t.Run(s.name, func(t *testing.T) {
			handleStepRequest(s, router, t)
		})
	}

	for _, s := range typeSteps {
		t.Run(s.name, func(t *testing.T) {
			handleStepRequest(s, router, t)
		})
	}
}

func TestTemplateService(t *testing.T) {
	ignoreOpenCensus := goleak.IgnoreTopFunction("go.opencensus.io/stats/view.(*worker).start")
	defer goleak.VerifyNone(t, ignoreOpenCensus)

	db, err := gorm.New()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(testDbDSN)
	defer gorm.Close(db)

	store := templates.NewGormStore(db)
	service := templates.NewService(store)

	// Add a token for testing
	token := templates.Token{Name: "RandomTokenName", Address: cfg.AdminAddress}
	err = service.AddToken(&token)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("Get token by name", func(t *testing.T) {
		t1, err := service.GetTokenByName("RandomTokenName")
		if err != nil {
			t.Fatal(err)
		}

		t2, err := service.GetTokenByName("randomtokenname")
		if err != nil {
			t.Fatal(err)
		}

		t3, err := service.GetTokenByName("randomTokenName")
		if err != nil {
			t.Fatal(err)
		}

		_, err = service.GetTokenByName("othername")
		if err == nil {
			t.Error("expected an error")
		}

		if t1.Address != token.Address || t2.Address != token.Address || t3.Address != token.Address {
			t.Error("expected tokens to be equal")
		}
	})

}
