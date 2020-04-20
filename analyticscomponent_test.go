package gocbcore

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

type analyticsTestHelper struct {
	TestName string
	NumDocs  int
	TestDocs *testDocs
}

func hlpRunAnalyticsQuery(t *testing.T, agent *Agent, opts AnalyticsQueryOptions) ([][]byte, error) {
	t.Helper()

	resCh := make(chan *AnalyticsRowReader, 1)
	errCh := make(chan error, 1)
	_, err := agent.AnalyticsQuery(opts, func(reader *AnalyticsRowReader, err error) {
		if err != nil {
			errCh <- err
			return
		}
		resCh <- reader
	})
	if err != nil {
		return nil, err
	}

	var rows *AnalyticsRowReader
	select {
	case err := <-errCh:
		return nil, err
	case res := <-resCh:
		rows = res
	}

	var rowBytes [][]byte
	for {
		row := rows.NextRow()
		if row == nil {
			break
		}

		rowBytes = append(rowBytes, row)
	}

	err = rows.Err()
	return rowBytes, err
}

func hlpEnsureDataset(t *testing.T, agent *Agent, bucketName string) {
	t.Helper()

	payloadStr := fmt.Sprintf("{\"statement\":\"CREATE DATASET IF NOT EXISTS `%s` ON `%s`\"}", bucketName, bucketName)
	_, err := hlpRunAnalyticsQuery(t, agent, AnalyticsQueryOptions{
		Payload:  []byte(payloadStr),
		Deadline: time.Now().Add(30000 * time.Millisecond),
	})
	if err != nil {
		t.Logf("Error occurred creating dataset: %s\n", err)
	}
	payloadStr = "{\"statement\":\"CONNECT LINK Local\"}"
	_, err = hlpRunAnalyticsQuery(t, agent, AnalyticsQueryOptions{
		Payload:  []byte(payloadStr),
		Deadline: time.Now().Add(30000 * time.Millisecond),
	})
	if err != nil {
		t.Logf("Error occurred connecting link: %s\n", err)
	}
}

func (nqh *analyticsTestHelper) testSetup(t *testing.T) {
	agent, h := testGetAgentAndHarness(t)

	nqh.TestDocs = makeTestDocs(t, agent, nqh.TestName, nqh.NumDocs)

	hlpEnsureDataset(t, agent, h.BucketName)
}

func (nqh *analyticsTestHelper) testCleanup(t *testing.T) {
	if nqh.TestDocs != nil {
		nqh.TestDocs.Remove()
		nqh.TestDocs = nil
	}
}

func (nqh *analyticsTestHelper) testBasic(t *testing.T) {
	agent, h := testGetAgentAndHarness(t)

	deadline := time.Now().Add(60000 * time.Millisecond)
	runTestQuery := func() ([]testDoc, error) {
		test := map[string]interface{}{
			"statement": fmt.Sprintf("SELECT i,testName FROM %s WHERE testName=\"%s\"", h.BucketName, nqh.TestName),
		}
		payload, err := json.Marshal(test)
		if err != nil {
			t.Errorf("failed to marshal test payload: %s", err)
		}

		iterDeadline := time.Now().Add(5000 * time.Millisecond)
		if iterDeadline.After(deadline) {
			iterDeadline = deadline
		}

		resCh := make(chan *AnalyticsRowReader)
		errCh := make(chan error)
		_, err = agent.AnalyticsQuery(AnalyticsQueryOptions{
			Payload:       payload,
			RetryStrategy: nil,
			Deadline:      iterDeadline,
		}, func(reader *AnalyticsRowReader, err error) {
			if err != nil {
				errCh <- err
				return
			}
			resCh <- reader
		})
		if err != nil {
			return nil, err
		}
		var rows *AnalyticsRowReader
		select {
		case err := <-errCh:
			return nil, err
		case res := <-resCh:
			rows = res
		}

		var docs []testDoc
		for {
			row := rows.NextRow()
			if row == nil {
				break
			}

			var doc testDoc
			err := json.Unmarshal(row, &doc)
			if err != nil {
				return nil, err
			}

			docs = append(docs, doc)
		}

		err = rows.Err()
		if err != nil {
			return nil, err
		}

		return docs, nil
	}

	lastError := ""
	for {
		docs, err := runTestQuery()
		if err == nil {
			testFailed := false

			for _, doc := range docs {
				if doc.I < 1 || doc.I > nqh.NumDocs {
					lastError = fmt.Sprintf("query test read invalid row i=%d", doc.I)
					testFailed = true
				}
			}

			numDocs := len(docs)
			if numDocs != nqh.NumDocs {
				lastError = fmt.Sprintf("query test read invalid number of rows %d!=%d", numDocs, 5)
				testFailed = true
			}

			if !testFailed {
				break
			}
		} else {
			t.Logf("Error occurred running analytics query, will retry: %s\n", err)
		}

		sleepDeadline := time.Now().Add(1000 * time.Millisecond)
		if sleepDeadline.After(deadline) {
			sleepDeadline = deadline
		}
		time.Sleep(sleepDeadline.Sub(time.Now()))

		if sleepDeadline == deadline {
			t.Errorf("timed out waiting for indexing: %s", lastError)
			break
		}
	}
}

func TestAnalytics(t *testing.T) {
	testEnsureSupportsFeature(t, TestFeatureCbas)

	helper := &analyticsTestHelper{
		TestName: "testAnalyticsQuery",
		NumDocs:  5,
	}

	if t.Run("setup", helper.testSetup) {
		t.Run("Basic", helper.testBasic)
	}

	t.Run("cleanup", helper.testCleanup)
}

func TestAnalyticsCancel(t *testing.T) {
	testEnsureSupportsFeature(t, TestFeatureCbas)

	agent, _ := testGetAgentAndHarness(t)

	rt := &roundTripper{delay: 1 * time.Second, tsport: agent.http.cli.Transport}
	httpCpt := newHTTPComponent(
		httpComponentProps{},
		&http.Client{Transport: rt},
		agent.httpMux,
		agent.http.auth,
		agent.tracer,
	)
	cbasCpt := newAnalyticsQueryComponent(httpCpt, &tracerComponent{tracer: noopTracer{}})

	resCh := make(chan *AnalyticsRowReader)
	errCh := make(chan error)
	payloadStr := `{"statement":"SELECT * FROM test LIMIT 1"}`
	op, err := cbasCpt.AnalyticsQuery(AnalyticsQueryOptions{
		Payload:  []byte(payloadStr),
		Deadline: time.Now().Add(5 * time.Second),
	}, func(reader *AnalyticsRowReader, err error) {
		if err != nil {
			errCh <- err
			return
		}
		resCh <- reader
	})
	if err != nil {
		t.Fatalf("Failed to execute query %s", err)
	}
	op.Cancel()

	var rows *AnalyticsRowReader
	var resErr error
	select {
	case err := <-errCh:
		resErr = err
	case res := <-resCh:
		rows = res
	}

	if rows != nil {
		t.Fatal("Received rows but should not have")
	}

	if !errors.Is(resErr, ErrRequestCanceled) {
		t.Fatalf("Error should have been request canceled but was %s", resErr)
	}
}

func TestAnalyticsTimeout(t *testing.T) {
	testEnsureSupportsFeature(t, TestFeatureCbas)

	agent, h := testGetAgentAndHarness(t)

	resCh := make(chan *AnalyticsRowReader)
	errCh := make(chan error)
	payloadStr := fmt.Sprintf(`{"statement":"SELECT * FROM %s LIMIT 1"}`, h.BucketName)
	_, err := agent.AnalyticsQuery(AnalyticsQueryOptions{
		Payload:  []byte(payloadStr),
		Deadline: time.Now().Add(100 * time.Microsecond),
	}, func(reader *AnalyticsRowReader, err error) {
		if err != nil {
			errCh <- err
			return
		}
		resCh <- reader
	})
	if err != nil {
		t.Fatalf("Failed to execute query %s", err)
	}

	var rows *AnalyticsRowReader
	var resErr error
	select {
	case err := <-errCh:
		resErr = err
	case res := <-resCh:
		rows = res
	}

	if rows != nil {
		t.Fatal("Received rows but should not have")
	}

	if !errors.Is(resErr, ErrTimeout) {
		t.Fatalf("Error should have been timeout but was %s", resErr)
	}
}