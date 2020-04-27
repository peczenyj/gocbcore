package gocbcore

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type testLogger struct {
	Parent   Logger
	LogCount []uint64
}

func (logger *testLogger) Log(level LogLevel, offset int, format string, v ...interface{}) error {
	if level >= 0 && level < LogMaxVerbosity {
		atomic.AddUint64(&logger.LogCount[level], 1)
	}

	return logger.Parent.Log(level, offset+1, fmt.Sprintf("[%s] ", logLevelToString(level))+format, v...)
}

func createTestLogger() *testLogger {
	return &testLogger{
		Parent:   VerboseStdioLogger(),
		LogCount: make([]uint64, LogMaxVerbosity),
	}
}

func envFlagString(envName, name, value, usage string) *string {
	envValue := os.Getenv(envName)
	if envValue != "" {
		value = envValue
	}
	return flag.String(name, value, usage)
}

func envFlagInt(envName, name string, value int, usage string) *int {
	envValue := os.Getenv(envName)
	if envValue != "" {
		var err error
		value, err = strconv.Atoi(envValue)
		if err != nil {
			panic("failed to parse string as int")
		}
	}
	return flag.Int(name, value, usage)
}

func envFlagBool(envName, name string, value bool, usage string) *bool {
	envValue := os.Getenv(envName)
	if envValue != "" {
		if envValue == "0" {
			value = false
		} else if strings.ToLower(envValue) == "false" {
			value = false
		} else {
			value = true
		}
	}
	return flag.Bool(name, value, usage)
}

func TestMain(m *testing.M) {
	initialGoroutineCount := runtime.NumGoroutine()

	testSuite := envFlagInt("GCBCTESTSUITE", "test-suite", 1,
		"The test suite to run, 1=standard,2=dcp")
	connStr := envFlagString("GCBCONNSTR", "connstr", "",
		"Connection string to run tests with")
	bucketName := envFlagString("GCBBUCKET", "bucket", "default",
		"The bucket to use to test against")
	disableLogger := envFlagBool("GCBNOLOG", "disable-logger", false,
		"Whether or not to disable the logger")
	username := envFlagString("GCBUSER", "user", "",
		"The username to use to authenticate when using a real server")
	password := envFlagString("GCBPASS", "pass", "",
		"The password to use to authenticate when using a real server")
	clusterVersionStr := envFlagString("GCBCVER", "cluster-version", "6.5.0",
		"The server version being tested against (major.minor.patch.build_edition)")
	featuresToTest := envFlagString("GCBFEAT", "features", "",
		"The features that should be tested")
	memdBucketName := envFlagString("GCBMEMDBUCKET", "memd-bucket", "memd",
		"The memd bucket to use to test against")
	scopeName := envFlagString("GCBSCOPE", "scope-name", "",
		"The scope name to use to test with collections")
	collectionName := envFlagString("GCBCOLL", "collection-name", "",
		"The collection name to use to test with collections")
	certsDir := envFlagString("GCBCERTSDIR", "certs-dir", "",
		"The path to the directory containing certificates with following names: ca.pem[,client.pem,client.key]")
	numMutations := envFlagInt("GCBDCPMUTATIONS", "dcp-num-mutations", 50,
		"The number of mutations to create")
	numDeletions := envFlagInt("GCBDCPDELETIONS", "dcp-num-deletions", 5,
		"The number of deletions to create")
	numExpirations := envFlagInt("GCBDCPEXPIRATIONS", "dcp-num-expirations", 5,
		"The number of expirations to create")
	flag.Parse()

	clusterVersion, err := nodeVersionFromString(*clusterVersionStr)
	if err != nil {
		panic("failed to parse specified cluster version")
	}

	featureFlags := ParseFeatureFlags(*featuresToTest)

	var authenticator AuthProvider
	var caProvider func() *x509.CertPool
	if len(*certsDir) > 0 {
		ca, cert, err := ParseCerts(*certsDir)
		if err != nil {
			panic("failed to parse certificates")
		}

		// Just because we have a root cert doesn't mean that we have client certs.
		if cert == nil {
			authenticator = &PasswordAuthProvider{
				Username: *username,
				Password: *password,
			}
		} else {
			authenticator = &CertificateAuthenticator{
				ClientCertificate: cert,
			}
		}
		caProvider = func() *x509.CertPool {
			return ca
		}
	} else {
		authenticator = &PasswordAuthProvider{
			Username: *username,
			Password: *password,
		}
	}

	if *testSuite == 1 {
		globalTestConfig = &TestConfig{
			ConnStr:        *connStr,
			BucketName:     *bucketName,
			MemdBucketName: *memdBucketName,
			ScopeName:      *scopeName,
			CollectionName: *collectionName,
			Authenticator:  authenticator,
			CAProvider:     caProvider,
			ClusterVersion: clusterVersion,
			FeatureFlags:   featureFlags,
		}
	} else if *testSuite == 2 {
		globalDCPTestConfig = &DCPTestConfig{
			ConnStr:        *connStr,
			BucketName:     *bucketName,
			Authenticator:  authenticator,
			CAProvider:     caProvider,
			ClusterVersion: clusterVersion,
			FeatureFlags:   featureFlags,
			NumMutations:   *numMutations,
			NumDeletions:   *numDeletions,
			NumExpirations: *numExpirations,
		}
	} else {
		panic("Unrecognized test suite requested")
	}

	var logger *testLogger
	if !*disableLogger {
		// Set up our special logger which logs the log level count
		logger = createTestLogger()
		SetLogger(logger)
	}

	result := m.Run()

	if logger != nil {
		log.Printf("Log Messages Emitted:")
		var preLogTotal uint64
		for i := 0; i < int(LogMaxVerbosity); i++ {
			count := atomic.LoadUint64(&logger.LogCount[i])
			preLogTotal += count
			log.Printf("  (%s): %d", logLevelToString(LogLevel(i)), count)
		}

		abnormalLogCount := atomic.LoadUint64(&logger.LogCount[LogError]) + atomic.LoadUint64(&logger.LogCount[LogWarn])
		if abnormalLogCount > 0 {
			log.Printf("Detected unexpected logging, failing")
			result = 1
		}

		time.Sleep(1 * time.Second)

		log.Printf("Post sleep log Messages Emitted:")
		var postLogTotal uint64
		for i := 0; i < int(LogMaxVerbosity); i++ {
			count := atomic.LoadUint64(&logger.LogCount[i])
			postLogTotal += count
			log.Printf("  (%s): %d", logLevelToString(LogLevel(i)), count)
		}

		if preLogTotal != postLogTotal {
			log.Printf("Detected unexpected logging after agent closed, failing")
			result = 1
		}
	}

	// Loop for at most a second checking for goroutines leaks, this gives any HTTP goroutines time to shutdown
	start := time.Now()
	var finalGoroutineCount int
	for time.Now().Sub(start) <= 1*time.Second {
		runtime.Gosched()
		finalGoroutineCount = runtime.NumGoroutine()
		if finalGoroutineCount == initialGoroutineCount {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if finalGoroutineCount != initialGoroutineCount {
		log.Printf("Detected a goroutine leak (%d before != %d after), failing", initialGoroutineCount, finalGoroutineCount)
		pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)
		result = 1
	} else {
		log.Printf("No goroutines appear to have leaked (%d before == %d after)", initialGoroutineCount, finalGoroutineCount)
	}

	os.Exit(result)
}

type CertificateAuthenticator struct {
	ClientCertificate *tls.Certificate
}

func (ca CertificateAuthenticator) SupportsTLS() bool {
	return true
}

func (ca CertificateAuthenticator) SupportsNonTLS() bool {
	return false
}

func (ca CertificateAuthenticator) Certificate(req AuthCertRequest) (*tls.Certificate, error) {
	return ca.ClientCertificate, nil
}

func (ca CertificateAuthenticator) Credentials(req AuthCredsRequest) ([]UserPassPair, error) {
	return []UserPassPair{{
		Username: "",
		Password: "",
	}}, nil
}
