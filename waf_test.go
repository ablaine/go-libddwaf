// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build (amd64 || arm64) && (linux || darwin)

package waf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"text/template"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
)

func TestHealth(t *testing.T) {
	require.NoError(t, Health())
}

func TestVersion(t *testing.T) {
	require.Regexp(t, `[0-9]+\.[0-9]+\.[0-9]+`, Version())
}

var testArachniRule = newArachniTestRule([]ruleInput{{Address: "server.request.headers.no_cookies", KeyPath: []string{"user-agent"}}}, nil)

var testArachniRuleTmpl = template.Must(template.New("").Parse(`
{
  "version": "2.1",
  "rules": [
	{
	  "id": "ua0-600-12x",
	  "name": "Arachni",
	  "tags": {
		"type": "security_scanner",
		"category": "attack_attempt"
	  },
	  "conditions": [
		{
		  "operator": "match_regex",
		  "parameters": {
			"inputs": [
			{{ range $i, $input := .Inputs -}}
			  {{ if gt $i 0 }},{{ end }}
				{ "address": "{{ $input.Address }}"{{ if ne (len $input.KeyPath) 0 }},  "key_path": [ {{ range $i, $path := $input.KeyPath }}{{ if gt $i 0 }}, {{ end }}"{{ $path }}"{{ end }} ]{{ end }} }
			{{- end }}
			],
			"regex": "^Arachni"
		  }
		}
	  ],
	  "transformers": []
	  {{- if .Actions }},
		"on_match": [
		{{ range $i, $action := .Actions -}}
		  {{ if gt $i 0 }},{{ end }}
		  "{{ $action }}"
		{{- end }}
		]
	  {{- end }}
	}
  ]
}
`))

type ruleInput struct {
	Address string
	KeyPath []string
}

func newArachniTestRule(inputs []ruleInput, actions []string) map[string]any {
	var buf bytes.Buffer
	if err := testArachniRuleTmpl.Execute(&buf, struct {
		Inputs  []ruleInput
		Actions []string
	}{Inputs: inputs, Actions: actions}); err != nil {
		panic(err)
	}
	parsed := map[string]any{}

	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		return nil
	}

	return parsed
}

func newDefaultHandle(rule any) (*Handle, error) {
	return NewHandle(rule, "", "")
}

func TestNewWAF(t *testing.T) {
	t.Run("valid-rule", func(t *testing.T) {
		waf, err := newDefaultHandle(testArachniRule)
		require.NoError(t, err)
		require.NotNil(t, waf)
		defer waf.Close()
	})

	t.Run("invalid-rule", func(t *testing.T) {
		// Test with a valid JSON but invalid rule format (field events should be an array)
		const rule = `
{
  "version": "2.1",
  "events": [
	{
	  "id": "ua0-600-12x",
	  "name": "Arachni",
	  "tags": {
		"type": "security_scanner"
	  },
	  "conditions": [
		{
		  "operation": "match_regex",
		  "parameters": {
			"inputs": [
			  { "address": "server.request.headers.no_cookies" }
			],
			"regex": "^Arachni"
		  }
		}
	  ],
	  "transformers": []
	}
  ]
}
`
		var parsed any

		require.NoError(t, json.Unmarshal([]byte(rule), &parsed))

		waf, err := newDefaultHandle(parsed)
		require.Error(t, err)
		require.Nil(t, waf)
	})
}

func TestMatching(t *testing.T) {

	waf, err := newDefaultHandle(newArachniTestRule([]ruleInput{{Address: "my.input"}}, nil))
	require.NoError(t, err)
	require.NotNil(t, waf)

	require.Equal(t, []string{"my.input"}, waf.Addresses())

	wafCtx := waf.NewContext()
	require.NotNil(t, wafCtx)

	// Not matching because the address value doesn't match the rule
	values := map[string]interface{}{
		"my.input": "go client",
	}
	matches, actions, err := wafCtx.Run(values, time.Second)
	require.NoError(t, err)
	require.Nil(t, matches)
	require.Nil(t, actions)

	// Not matching because the address is not used by the rule
	values = map[string]interface{}{
		"server.request.uri.raw": "something",
	}
	matches, actions, err = wafCtx.Run(values, time.Second)
	require.NoError(t, err)
	require.Nil(t, matches)
	require.Nil(t, actions)

	// Not matching due to a timeout
	values = map[string]interface{}{
		"my.input": "Arachni",
	}
	matches, actions, err = wafCtx.Run(values, 0)
	require.Equal(t, ErrTimeout, err)
	require.Nil(t, matches)
	require.Nil(t, actions)

	// Matching
	// Note a WAF rule can only match once. This is why we test the matching case at the end.
	values = map[string]interface{}{
		"my.input": "Arachni",
	}
	matches, actions, err = wafCtx.Run(values, time.Second)
	require.NoError(t, err)
	require.NotEmpty(t, matches)
	require.Nil(t, actions)

	// Not matching anymore since it already matched before
	matches, actions, err = wafCtx.Run(values, time.Second)
	require.NoError(t, err)
	require.Nil(t, matches)
	require.Nil(t, actions)

	// Nil values
	matches, actions, err = wafCtx.Run(nil, time.Second)
	require.NoError(t, err)
	require.Nil(t, matches)
	require.Nil(t, actions)

	// Empty values
	matches, actions, err = wafCtx.Run(map[string]interface{}{}, time.Second)
	require.NoError(t, err)
	require.Nil(t, matches)
	require.Nil(t, actions)

	wafCtx.Close()
	waf.Close()
	// Using the WAF instance after it was closed leads to a nil WAF context
	require.Nil(t, waf.NewContext())
}

func TestActions(t *testing.T) {
	testActions := func(expectedActions []string) func(t *testing.T) {
		return func(t *testing.T) {

			waf, err := newDefaultHandle(newArachniTestRule([]ruleInput{{Address: "my.input"}}, expectedActions))
			require.NoError(t, err)
			require.NotNil(t, waf)
			defer waf.Close()

			wafCtx := waf.NewContext()
			require.NotNil(t, wafCtx)
			defer wafCtx.Close()

			// Not matching because the address value doesn't match the rule
			values := map[string]interface{}{
				"my.input": "Arachni",
			}
			matches, actions, err := wafCtx.Run(values, time.Second)
			require.NoError(t, err)
			require.NotEmpty(t, matches)
			// FIXME: check with libddwaf why the order of returned actions is not kept the same
			require.ElementsMatch(t, expectedActions, actions)
		}
	}

	t.Run("single", testActions([]string{"block"}))
	t.Run("multiple-actions", testActions([]string{"action 1", "action 2", "action 3"}))
}

func TestAddresses(t *testing.T) {
	expectedAddresses := []string{"my.first.input", "my.second.input", "my.indexed.input", "my.third.input"}
	addresses := []ruleInput{{Address: "my.first.input"}, {Address: "my.second.input"}, {Address: "my.third.input"}, {Address: "my.indexed.input", KeyPath: []string{"indexed"}}}
	waf, err := newDefaultHandle(newArachniTestRule(addresses, nil))
	require.NoError(t, err)
	defer waf.Close()
	require.Equal(t, expectedAddresses, waf.Addresses())
}

func TestConcurrency(t *testing.T) {
	// Start 800 goroutines that will use the WAF 500 times each
	nbUsers := 50
	nbRun := 500

	t.Run("concurrent-waf-context-usage", func(t *testing.T) {
		waf, err := newDefaultHandle(testArachniRule)
		require.NoError(t, err)
		defer waf.Close()

		wafCtx := waf.NewContext()
		defer wafCtx.Close()

		// User agents that won't match the rule so that it doesn't get pruned.
		// Said otherwise, the User-Agent rule will run as long as it doesn't match, otherwise it gets ignored.
		// This is the reason why the following user agent are not Arachni.
		userAgents := [...]string{"Foo", "Bar", "Datadog"}
		length := len(userAgents)

		var startBarrier, stopBarrier sync.WaitGroup
		// Create a start barrier to synchronize every goroutine's launch and
		// increase the chances of parallel accesses
		startBarrier.Add(1)
		// Create a stopBarrier to signal when all user goroutines are done.
		stopBarrier.Add(nbUsers)

		for n := 0; n < nbUsers; n++ {
			go func() {
				startBarrier.Wait()      // Sync the starts of the goroutines
				defer stopBarrier.Done() // Signal we are done when returning

				for c := 0; c < nbRun; c++ {
					i := c % length
					data := map[string]interface{}{
						"server.request.headers.no_cookies": map[string]string{
							"user-agent": userAgents[i],
						},
					}
					matches, _, err := wafCtx.Run(data, time.Minute)
					if err != nil {
						panic(err)
					}
					if len(matches) > 0 {
						panic(fmt.Errorf("c=%d matches=`%v`", c, string(matches)))
					}
				}
			}()
		}

		// Save the test start time to compare it to the first metrics store's
		// that should be latter.
		startBarrier.Done() // Unblock the user goroutines
		stopBarrier.Wait()  // Wait for the user goroutines to be done

		// Test the rule matches Arachni in the end
		data := map[string]interface{}{
			"server.request.headers.no_cookies": map[string]string{
				"user-agent": "Arachni",
			},
		}
		matches, _, err := wafCtx.Run(data, time.Second)
		require.NoError(t, err)
		require.NotEmpty(t, matches)
	})

	t.Run("concurrent-waf-instance-usage", func(t *testing.T) {
		waf, err := newDefaultHandle(testArachniRule)
		require.NoError(t, err)
		defer waf.Close()

		// User agents that won't match the rule so that it doesn't get pruned.
		// Said otherwise, the User-Agent rule will run as long as it doesn't match, otherwise it gets ignored.
		// This is the reason why the following user agent are not Arachni.
		userAgents := [...]string{"Foo", "Bar", "Datadog"}
		length := len(userAgents)

		var startBarrier, stopBarrier sync.WaitGroup
		// Create a start barrier to synchronize every goroutine's launch and
		// increase the chances of parallel accesses
		startBarrier.Add(1)
		// Create a stopBarrier to signal when all user goroutines are done.
		stopBarrier.Add(nbUsers)

		for n := 0; n < nbUsers; n++ {
			go func() {
				startBarrier.Wait()      // Sync the starts of the goroutines
				defer stopBarrier.Done() // Signal we are done when returning

				wafCtx := waf.NewContext()
				defer wafCtx.Close()

				for c := 0; c < nbRun; c++ {
					i := c % length
					data := map[string]interface{}{
						"server.request.headers.no_cookies": map[string]string{
							"user-agent": userAgents[i],
						},
					}

					matches, _, err := wafCtx.Run(data, time.Minute)

					if err != nil {
						panic(err)
					}
					if len(matches) > 0 {
						panic(fmt.Errorf("c=%d matches=`%v`", c, string(matches)))
					}
				}

				// Test the rule matches Arachni in the end
				data := map[string]interface{}{
					"server.request.headers.no_cookies": map[string]string{
						"user-agent": "Arachni",
					},
				}
				matches, actions, err := wafCtx.Run(data, time.Second)
				require.NoError(t, err)
				require.NotEmpty(t, matches)
				require.Nil(t, actions)
			}()
		}

		// Save the test start time to compare it to the first metrics store's
		// that should be latter.
		startBarrier.Done() // Unblock the user goroutines
		stopBarrier.Wait()  // Wait for the user goroutines to be done
	})
}

func TestRunError(t *testing.T) {
	for _, tc := range []struct {
		Err            error
		ExpectedString string
	}{
		{
			Err:            ErrInternal,
			ExpectedString: "internal waf error",
		},
		{
			Err:            ErrTimeout,
			ExpectedString: "waf timeout",
		},
		{
			Err:            ErrInvalidObject,
			ExpectedString: "invalid waf object",
		},
		{
			Err:            ErrInvalidArgument,
			ExpectedString: "invalid waf argument",
		},
		{
			Err:            ErrOutOfMemory,
			ExpectedString: "out of memory",
		},
		{
			Err:            RunError(33),
			ExpectedString: "unknown waf error 33",
		},
	} {
		t.Run(tc.ExpectedString, func(t *testing.T) {
			require.Equal(t, tc.ExpectedString, tc.Err.Error())
		})
	}
}

func TestMetrics(t *testing.T) {
	rules := `{
  "version": "2.1",
  "metadata": {
	"rules_version": "1.2.7"
  },
  "rules": [
	{
	  "id": "valid-rule",
	  "name": "Unicode Full/Half Width Abuse Attack Attempt",
	  "tags": {
		"type": "http_protocol_violation"
	  },
	  "conditions": [
		{
		  "parameters": {
			"inputs": [
			  {
				"address": "server.request.uri.raw"
			  }
			],
			"regex": "\\%u[fF]{2}[0-9a-fA-F]{2}"
		  },
		  "operator": "match_regex"
		}
	  ],
	  "transformers": []
	},
	{
	  "id": "missing-tags-1",
	  "name": "Unicode Full/Half Width Abuse Attack Attempt",
	  "conditions": [
	  ],
	  "transformers": []
	},
	{
	  "id": "missing-tags-2",
	  "name": "Unicode Full/Half Width Abuse Attack Attempt",
	  "conditions": [
	  ],
	  "transformers": []
	},
	{
	  "id": "missing-name",
	  "tags": {
		"type": "http_protocol_violation"
	  },
	  "conditions": [
	  ],
	  "transformers": []
	}
  ]
}
`
	var parsed any

	require.NoError(t, json.Unmarshal([]byte(rules), &parsed))

	waf, err := newDefaultHandle(parsed)
	require.NoError(t, err)
	defer waf.Close()
	// TODO: (Francois Mazeau) see if we can make this test more configurable to future proof against libddwaf changes
	t.Run("RulesetInfo", func(t *testing.T) {
		rInfo := waf.RulesetInfo()
		require.Equal(t, uint16(3), rInfo.Failed)
		require.Equal(t, uint16(1), rInfo.Loaded)
		require.Equal(t, 2, len(rInfo.Errors))
		require.Equal(t, "1.2.7", rInfo.Version)
		require.Equal(t, map[string][]string{
			"missing key 'tags'": {"missing-tags-1", "missing-tags-2"},
			"missing key 'name'": {"missing-name"},
		}, rInfo.Errors)
	})

	t.Run("RunDuration", func(t *testing.T) {
		wafCtx := waf.NewContext()
		require.NotNil(t, wafCtx)
		defer wafCtx.Close()
		// Craft matching data to force work on the WAF
		data := map[string]interface{}{
			"server.request.uri.raw": "\\%uff00",
		}
		start := time.Now()
		matches, actions, err := wafCtx.Run(data, time.Second)
		elapsedNS := time.Since(start).Nanoseconds()
		require.NoError(t, err)
		require.NotNil(t, matches)
		require.Nil(t, actions)

		// Make sure that WAF runtime was set
		overall, internal := wafCtx.TotalRuntime()
		require.Greater(t, overall, uint64(0))
		require.Greater(t, internal, uint64(0))
		require.Greater(t, overall, internal)
		require.LessOrEqual(t, overall, uint64(elapsedNS))
	})

	t.Run("Timeouts", func(t *testing.T) {
		wafCtx := waf.NewContext()
		require.NotNil(t, wafCtx)
		defer wafCtx.Close()
		// Craft matching data to force work on the WAF
		data := map[string]interface{}{
			"server.request.uri.raw": "\\%uff00",
		}

		for i := uint64(1); i <= 10; i++ {
			_, _, err := wafCtx.Run(data, time.Nanosecond)
			require.Equal(t, err, ErrTimeout)
			require.Equal(t, i, wafCtx.TotalTimeouts())
		}
	})
}

func TestEncoder(t *testing.T) {
	for _, tc := range []struct {
		Name                   string
		Data                   interface{}
		ExpectedError          error
		ExpectedWAFValueType   int
		ExpectedWAFValueLength int
		ExpectedWAFString      string
		MaxValueDepth          interface{}
		MaxContainerLength     interface{}
		MaxStringLength        interface{}
	}{
		{
			Name:          "unsupported type",
			Data:          make(chan struct{}),
			ExpectedError: errUnsupportedValue,
		},
		{
			Name:              "string-hekki",
			Data:              "hello, waf",
			ExpectedWAFString: "hello, waf",
		},
		{
			Name:                   "string-empty",
			Data:                   "",
			ExpectedWAFValueType:   wafStringType,
			ExpectedWAFValueLength: 0,
		},
		{
			Name:              "byte-slice",
			Data:              []byte("hello, waf"),
			ExpectedWAFString: "hello, waf",
		},
		{
			Name:                   "nil-byte-slice",
			Data:                   []byte(nil),
			ExpectedWAFValueType:   wafStringType,
			ExpectedWAFValueLength: 0,
		},
		{
			Name:                   "map-with-empty-key-string",
			Data:                   map[string]int{"": 1},
			ExpectedWAFValueType:   wafMapType,
			ExpectedWAFValueLength: 1,
		},
		{
			Name:                   "empty-struct",
			Data:                   struct{}{},
			ExpectedWAFValueType:   wafMapType,
			ExpectedWAFValueLength: 0,
		},
		{
			Name: "empty-struct-with-private-fields",
			Data: struct {
				a string
				b int
				c bool
			}{},
			ExpectedWAFValueType:   wafMapType,
			ExpectedWAFValueLength: 0,
		},
		{
			Name:          "nil-interface-value",
			Data:          nil,
			ExpectedError: errUnsupportedValue,
		},
		{
			Name:          "nil-pointer-value",
			Data:          (*string)(nil),
			ExpectedError: errUnsupportedValue,
		},
		{
			Name:          "nil-pointer-value",
			Data:          (*int)(nil),
			ExpectedError: errUnsupportedValue,
		},
		{
			Name:              "non-nil-pointer-value",
			Data:              new(int),
			ExpectedWAFString: "0",
		},
		{
			Name:                   "non-nil-pointer-value",
			Data:                   new(string),
			ExpectedWAFValueType:   wafStringType,
			ExpectedWAFValueLength: 0,
		},
		{
			Name:                   "having-an-empty-map",
			Data:                   map[string]interface{}{},
			ExpectedWAFValueType:   wafMapType,
			ExpectedWAFValueLength: 0,
		},
		{
			Name:          "unsupported",
			Data:          func() {},
			ExpectedError: errUnsupportedValue,
		},
		{
			Name:              "int",
			Data:              int(1234),
			ExpectedWAFString: "1234",
		},
		{
			Name:              "uint",
			Data:              uint(9876),
			ExpectedWAFString: "9876",
		},
		{
			Name:              "bool",
			Data:              true,
			ExpectedWAFString: "true",
		},
		{
			Name:              "bool",
			Data:              false,
			ExpectedWAFString: "false",
		},
		{
			Name:              "float",
			Data:              33.12345,
			ExpectedWAFString: "33",
		},
		{
			Name:              "float",
			Data:              33.62345,
			ExpectedWAFString: "34",
		},
		{
			Name:                   "slice",
			Data:                   []interface{}{33.12345, "ok", 27},
			ExpectedWAFValueType:   wafArrayType,
			ExpectedWAFValueLength: 3,
		},
		{
			Name:                   "slice-having-unsupported-values",
			Data:                   []interface{}{33.12345, func() {}, "ok", 27, nil},
			ExpectedWAFValueType:   wafArrayType,
			ExpectedWAFValueLength: 3,
		},
		{
			Name:                   "array",
			Data:                   [...]interface{}{func() {}, 33.12345, "ok", 27},
			ExpectedWAFValueType:   wafArrayType,
			ExpectedWAFValueLength: 3,
		},
		{
			Name:                   "map",
			Data:                   map[string]interface{}{"k1": 1, "k2": "2"},
			ExpectedWAFValueType:   wafMapType,
			ExpectedWAFValueLength: 2,
		},
		{
			Name:                   "map-with-unsupported-key-values",
			Data:                   map[interface{}]interface{}{"k1": 1, 27: "int key", "k2": "2"},
			ExpectedWAFValueType:   wafMapType,
			ExpectedWAFValueLength: 2,
		},
		{
			Name:                   "map-with-indirect-key-string-values",
			Data:                   map[interface{}]interface{}{"k1": 1, new(string): "string pointer key", "k2": "2"},
			ExpectedWAFValueType:   wafMapType,
			ExpectedWAFValueLength: 3,
		},
		{
			Name: "struct",
			Data: struct {
				Public  string
				private string
				a       string
				A       string
			}{
				Public:  "Public",
				private: "private",
				a:       "a",
				A:       "A",
			},
			ExpectedWAFValueType:   wafMapType,
			ExpectedWAFValueLength: 2, // public fields only
		},
		{
			Name: "struct-with-unsupported-values",
			Data: struct {
				Public  string
				private string
				a       string
				A       func()
			}{
				Public:  "Public",
				private: "private",
				a:       "a",
				A:       nil,
			},
			ExpectedWAFValueType:   wafMapType,
			ExpectedWAFValueLength: 1, // public fields of supported types
		},
		{
			Name:                   "array-max-depth",
			MaxValueDepth:          0,
			Data:                   []interface{}{1, 2, 3, 4, []int{1, 2, 3, 4}},
			ExpectedWAFValueType:   wafArrayType,
			ExpectedWAFValueLength: 4,
		},
		{
			Name:                   "array-max-depth",
			MaxValueDepth:          1,
			Data:                   []interface{}{1, 2, 3, 4, []int{1, 2, 3, 4}},
			ExpectedWAFValueType:   wafArrayType,
			ExpectedWAFValueLength: 5,
		},
		{
			Name:                   "array-max-depth",
			MaxValueDepth:          0,
			Data:                   []interface{}{},
			ExpectedWAFValueType:   wafArrayType,
			ExpectedWAFValueLength: 0,
		},
		{
			Name:                   "map-max-depth",
			MaxValueDepth:          0,
			Data:                   map[string]interface{}{"k1": "v1", "k2": "v2", "k3": "v3", "k4": "v4", "k5": map[string]string{}},
			ExpectedWAFValueType:   wafMapType,
			ExpectedWAFValueLength: 4,
		},
		{
			Name:                   "map-max-depth",
			MaxValueDepth:          1,
			Data:                   map[string]interface{}{"k1": "v1", "k2": "v2", "k3": "v3", "k4": "v4", "k5": map[string]string{}},
			ExpectedWAFValueType:   wafMapType,
			ExpectedWAFValueLength: 5,
		},
		{
			Name:                   "map-max-depth",
			MaxValueDepth:          0,
			Data:                   map[string]interface{}{},
			ExpectedWAFValueType:   wafMapType,
			ExpectedWAFValueLength: 0,
		},
		{
			Name:                   "struct-max-depth",
			MaxValueDepth:          0,
			Data:                   struct{}{},
			ExpectedWAFValueType:   wafMapType,
			ExpectedWAFValueLength: 0,
		},
		{
			Name:          "struct-max-depth",
			MaxValueDepth: 0,
			Data: struct {
				F0 string
				F1 struct{}
			}{},
			ExpectedWAFValueType:   wafMapType,
			ExpectedWAFValueLength: 1,
		},
		{
			Name:          "struct-max-depth",
			MaxValueDepth: 1,
			Data: struct {
				F0 string
				F1 struct{}
			}{},
			ExpectedWAFValueType:   wafMapType,
			ExpectedWAFValueLength: 2,
		},
		{
			Name:              "scalar-values-max-depth-not-accounted",
			MaxValueDepth:     0,
			Data:              -1234,
			ExpectedWAFString: "-1234",
		},
		{
			Name:              "scalar-values-max-depth-not-accounted",
			MaxValueDepth:     0,
			Data:              uint(1234),
			ExpectedWAFString: "1234",
		},
		{
			Name:              "scalar-values-max-depth-not-accounted",
			MaxValueDepth:     0,
			Data:              false,
			ExpectedWAFString: "false",
		},
		{
			Name:                   "array-max-length",
			MaxContainerLength:     3,
			Data:                   []interface{}{1, 2, 3, 4, 5},
			ExpectedWAFValueType:   wafArrayType,
			ExpectedWAFValueLength: 3,
		},
		{
			Name:                   "map-max-length",
			MaxContainerLength:     3,
			Data:                   map[string]string{"k1": "v1", "k2": "v2", "k3": "v3", "k4": "v4", "k5": "v5"},
			ExpectedWAFValueType:   wafMapType,
			ExpectedWAFValueLength: 3,
		},
		{
			Name:              "string-max-length",
			MaxStringLength:   3,
			Data:              "123456789",
			ExpectedWAFString: "123",
		},
		{
			Name:                   "string-max-length-truncation-leading-to-same-map-keys",
			MaxStringLength:        1,
			Data:                   map[string]string{"k1": "v1", "k2": "v2", "k3": "v3", "k4": "v4", "k5": "v5"},
			ExpectedWAFValueType:   wafMapType,
			ExpectedWAFValueLength: 5,
		},
		{
			Name:                   "unsupported-array-values",
			MaxContainerLength:     1,
			Data:                   []interface{}{"supported", func() {}, "supported", make(chan struct{})},
			ExpectedWAFValueType:   wafArrayType,
			ExpectedWAFValueLength: 1,
		},
		{
			Name:                   "unsupported-array-values",
			MaxContainerLength:     2,
			Data:                   []interface{}{"supported", func() {}, "supported", make(chan struct{})},
			ExpectedWAFValueType:   wafArrayType,
			ExpectedWAFValueLength: 2,
		},
		{
			Name:                   "unsupported-array-values",
			MaxContainerLength:     1,
			Data:                   []interface{}{func() {}, "supported", make(chan struct{}), "supported"},
			ExpectedWAFValueType:   wafArrayType,
			ExpectedWAFValueLength: 1,
		},
		{
			Name:                   "unsupported-array-values",
			MaxContainerLength:     2,
			Data:                   []interface{}{func() {}, "supported", make(chan struct{}), "supported"},
			ExpectedWAFValueType:   wafArrayType,
			ExpectedWAFValueLength: 2,
		},
		{
			Name:                   "unsupported-array-values",
			MaxContainerLength:     1,
			Data:                   []interface{}{func() {}, make(chan struct{}), "supported"},
			ExpectedWAFValueType:   wafArrayType,
			ExpectedWAFValueLength: 1,
		},
		{
			Name:                   "unsupported-array-values",
			MaxContainerLength:     3,
			Data:                   []interface{}{"supported", func() {}, make(chan struct{})},
			ExpectedWAFValueType:   wafArrayType,
			ExpectedWAFValueLength: 1,
		},
		{
			Name:                   "unsupported-array-values",
			MaxContainerLength:     3,
			Data:                   []interface{}{func() {}, "supported", make(chan struct{})},
			ExpectedWAFValueType:   wafArrayType,
			ExpectedWAFValueLength: 1,
		},
		{
			Name:                   "unsupported-array-values",
			MaxContainerLength:     3,
			Data:                   []interface{}{func() {}, make(chan struct{}), "supported"},
			ExpectedWAFValueType:   wafArrayType,
			ExpectedWAFValueLength: 1,
		},
		{
			Name:                   "unsupported-array-values",
			MaxContainerLength:     2,
			Data:                   []interface{}{func() {}, make(chan struct{}), "supported", "supported"},
			ExpectedWAFValueType:   wafArrayType,
			ExpectedWAFValueLength: 2,
		},
		{
			Name: "unsupported-map-key-types",
			Data: map[interface{}]int{
				"supported":           1,
				interface{ m() }(nil): 1,
				nil:                   1,
				(*int)(nil):           1,
				(*string)(nil):        1,
				make(chan struct{}):   1,
			},
			ExpectedWAFValueType:   wafMapType,
			ExpectedWAFValueLength: 1,
		},
		{
			Name: "unsupported-map-key-types",
			Data: map[interface{}]int{
				interface{ m() }(nil): 1,
				nil:                   1,
				(*int)(nil):           1,
				(*string)(nil):        1,
				make(chan struct{}):   1,
			},
			ExpectedWAFValueType:   wafMapType,
			ExpectedWAFValueLength: 0,
		},
		{
			Name: "unsupported-map-values",
			Data: map[string]interface{}{
				"k0": "supported",
				"k1": func() {},
				"k2": make(chan struct{}),
			},
			MaxContainerLength:     3,
			ExpectedWAFValueLength: 1,
		},
		{
			Name: "unsupported-map-values",
			Data: map[string]interface{}{
				"k0": "supported",
				"k1": "supported",
				"k2": make(chan struct{}),
			},
			MaxContainerLength:     3,
			ExpectedWAFValueLength: 2,
		},
		{
			Name: "unsupported-map-values",
			Data: map[string]interface{}{
				"k0": "supported",
				"k1": "supported",
				"k2": make(chan struct{}),
			},
			MaxContainerLength:     1,
			ExpectedWAFValueLength: 1,
		},
		{
			Name: "unsupported-struct-values",
			Data: struct {
				F0 string
				F1 func()
				F2 chan struct{}
			}{
				F0: "supported",
				F1: func() {},
				F2: make(chan struct{}),
			},
			MaxContainerLength:     3,
			ExpectedWAFValueLength: 1,
		},
		{
			Name: "unsupported-map-values",
			Data: struct {
				F0 string
				F1 string
				F2 chan struct{}
			}{
				F0: "supported",
				F1: "supported",
				F2: make(chan struct{}),
			},
			MaxContainerLength:     3,
			ExpectedWAFValueLength: 2,
		},
		{
			Name: "unsupported-map-values",
			Data: struct {
				F0 string
				F1 string
				F2 chan struct{}
			}{
				F0: "supported",
				F1: "supported",
				F2: make(chan struct{}),
			},
			MaxContainerLength:     1,
			ExpectedWAFValueLength: 1,
		},
	} {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			maxValueDepth := 10
			if max := tc.MaxValueDepth; max != nil {
				maxValueDepth = max.(int)
			}
			maxContainerLength := 1000
			if max := tc.MaxContainerLength; max != nil {
				maxContainerLength = max.(int)
			}
			maxStringLength := 4096
			if max := tc.MaxStringLength; max != nil {
				maxStringLength = max.(int)
			}
			e := encoder{
				objectMaxDepth:   maxValueDepth,
				stringMaxSize:    maxStringLength,
				containerMaxSize: maxContainerLength,
			}
			wo, err := e.Encode(tc.Data)
			if tc.ExpectedError != nil {
				require.Error(t, err)
				require.Equal(t, tc.ExpectedError, err)
				require.Nil(t, wo)
				return
			}

			require.NoError(t, err)
			require.NotEqual(t, &wafObject{}, wo)

			if tc.ExpectedWAFValueType != 0 {
				require.Equal(t, tc.ExpectedWAFValueType, int(wo._type), "bad waf value type")
			}
			if tc.ExpectedWAFValueLength != 0 {
				require.Equal(t, tc.ExpectedWAFValueLength, int(wo.nbEntries), "bad waf value length")
			}
			if expectedStr := tc.ExpectedWAFString; expectedStr != "" {
				require.Equal(t, wafStringType, int(wo._type), "bad waf string value type")
				cbuf := wo.value
				gobuf := []byte(expectedStr)
				require.Equal(t, len(gobuf), int(wo.nbEntries), "bad waf value length")
				for i, gobyte := range gobuf {
					// Go pointer arithmetic for cbyte := cbuf[i]
					cbyte := *(*uint8)(unsafe.Pointer(cbuf + uintptr(i))) //nolint:govet
					if cbyte != gobyte {
						t.Fatalf("bad waf string value content: i=%d cbyte=%d gobyte=%d", i, cbyte, gobyte)
					}
				}
			}

			// Pass the encoded value to the WAF to make sure it doesn't return an error
			waf, err := newDefaultHandle(newArachniTestRule([]ruleInput{{Address: "my.input"}}, nil))
			require.NoError(t, err)
			defer waf.Close()
			wafCtx := waf.NewContext()
			require.NotNil(t, wafCtx)
			defer wafCtx.Close()
			_, _, err = wafCtx.Run(map[string]interface{}{
				"my.input": tc.Data,
			}, time.Second)
			require.NoError(t, err)
		})
	}
}

/* This test needs a working encoder to function properly, as it first encodes the objects before decoding them
func TestDecoder(t *testing.T) {
	e := newMaxEncoder()
	objBuilder := func(v interface{}) *wafObject {
		var err error
		obj := &wafObject{}
		// Right now the encoder encodes integer values as strings to match the WAF representation.
		// We circumvent this here by manually encoding so that we can test with WAF objects that hold real integers,
		// not string representations of integers. See https://github.com/DataDog/libddwaf/issues/41.
		if v, ok := v.(int64); ok {
			obj.setInt64(toCInt64(int(v)))
			return obj
		}
		if v, ok := v.(uint64); ok {
			obj.setUint64(toCUint64(uint(v)))
			return obj
		}
		obj, err = e.encode(v)
		require.NoError(t, err, "Encoding object failed")
		return obj
	}

	t.Run("Valid", func(t *testing.T) {
		for _, tc := range []struct {
			Name          string
			Object        *wafObject
			ExpectedValue interface{}
		}{
			{
				Name:          "string",
				ExpectedValue: "string",
				Object:        objBuilder("string"),
			},
			{
				Name:          "empty-string",
				ExpectedValue: "",
				Object:        objBuilder(""),
			},
			{
				Name:          "uint64",
				ExpectedValue: uint64(42),
				Object:        objBuilder(uint64(42)),
			},
			{
				Name:          "int64",
				ExpectedValue: int64(42),
				Object:        objBuilder(int64(42)),
			},
			{
				Name:          "array",
				ExpectedValue: []interface{}{"str1", "str2", "str3", "str4"},
				Object:        objBuilder([]string{"str1", "str2", "str3", "str4"}),
			},
			{
				Name:          "empty-array",
				ExpectedValue: []interface{}{},
				Object:        objBuilder([]interface{}{}),
			},
			{
				Name:          "struct",
				ExpectedValue: map[string]interface{}{"Str": "string"},
				Object: objBuilder(struct {
					Str string
				}{Str: "string"}),
			},
			{
				Name:          "empty-struct",
				ExpectedValue: map[string]interface{}{},
				Object:        objBuilder(struct{}{}),
			},
			{
				Name:          "map",
				ExpectedValue: map[string]interface{}{"foo": "bar", "bar": "baz", "baz": "foo"},
				Object:        objBuilder(map[string]interface{}{"foo": "bar", "bar": "baz", "baz": "foo"}),
			},
			{
				Name:          "empty-map",
				ExpectedValue: map[string]interface{}{},
				Object:        objBuilder(map[string]interface{}{}),
			},
			{
				Name:          "nested",
				ExpectedValue: []interface{}{"1", "2", map[string]interface{}{"foo": "bar", "bar": "baz", "baz": "foo"}, []interface{}{"1", "2", "3"}},
				Object:        objBuilder([]interface{}{1, "2", map[string]string{"foo": "bar", "bar": "baz", "baz": "foo"}, []int{1, 2, 3}}),
			},
		} {
			tc := tc
			t.Run(tc.Name, func(t *testing.T) {
				val, err := decodeObject(tc.Object)
				require.NoErrorf(t, err, "Error decoding the object: %v", err)
				require.Equal(t, reflect.TypeOf(tc.ExpectedValue), reflect.TypeOf(val))
				require.Equal(t, tc.ExpectedValue, val)
			})
		}
	})

	t.Run("Invalid", func(t *testing.T) {
		for _, tc := range []struct {
			Name          string
			Object        *wafObject
			Modifier      func(object *wafObject)
			ExpectedError error
		}{
			{
				Name:          "WAF-object",
				Object:        nil,
				ExpectedError: errNilObjectPtr,
			},
			{
				Name:          "type",
				Object:        objBuilder("obj"),
				Modifier:      func(object *wafObject) { object._type = 5 },
				ExpectedError: errUnsupportedValue,
			},
			{
				Name:          "map-key-1",
				Object:        objBuilder(map[string]interface{}{"baz": "foo"}),
				Modifier:      func(object *wafObject) { object.index(0).setMapKey(nil, 0) },
				ExpectedError: errInvalidMapKey,
			},
			{
				Name:          "map-key-2",
				Object:        objBuilder(map[string]interface{}{"baz": "foo"}),
				Modifier:      func(object *wafObject) { object.index(0).setMapKey(nil, 10) },
				ExpectedError: errInvalidMapKey,
			},
			{
				Name:          "array-ptr",
				Object:        objBuilder([]interface{}{"foo"}),
				Modifier:      func(object *wafObject) { *object.arrayValuePtr() = nil },
				ExpectedError: errNilObjectPtr,
			},
			{
				Name:          "map-ptr",
				Object:        objBuilder(map[string]interface{}{"baz": "foo"}),
				Modifier:      func(object *wafObject) { *object.arrayValuePtr() = nil },
				ExpectedError: errNilObjectPtr,
			},
		} {
			tc := tc
			t.Run(tc.Name, func(t *testing.T) {
				if tc.Modifier != nil {
					tc.Modifier(tc.Object)
				}
				_, err := decodeObject(tc.Object)
				if tc.ExpectedError != nil {
					require.Equal(t, tc.ExpectedError, err)
				} else {
					require.Error(t, err)
				}

			})
		}
	})
}*/

func TestObfuscatorConfig(t *testing.T) {
	rule := newArachniTestRule([]ruleInput{{Address: "my.addr", KeyPath: []string{"key"}}}, nil)
	t.Run("key", func(t *testing.T) {
		waf, err := NewHandle(rule, "key", "")
		require.NoError(t, err)
		defer waf.Close()
		wafCtx := waf.NewContext()
		require.NotNil(t, wafCtx)
		defer wafCtx.Close()
		data := map[string]interface{}{
			"my.addr": map[string]interface{}{"key": "Arachni-sensitive-Arachni"},
		}
		matches, actions, err := wafCtx.Run(data, time.Second)
		require.NotNil(t, matches)
		require.Nil(t, actions)
		require.NoError(t, err)
		require.NotContains(t, (string)(matches), "sensitive")
	})

	t.Run("val", func(t *testing.T) {
		waf, err := NewHandle(rule, "", "sensitive")
		require.NoError(t, err)
		defer waf.Close()
		wafCtx := waf.NewContext()
		require.NotNil(t, wafCtx)
		defer wafCtx.Close()
		data := map[string]interface{}{
			"my.addr": map[string]interface{}{"key": "Arachni-sensitive-Arachni"},
		}
		matches, actions, err := wafCtx.Run(data, time.Second)
		require.NotNil(t, matches)
		require.Nil(t, actions)
		require.NoError(t, err)
		require.NotContains(t, (string)(matches), "sensitive")
	})

	t.Run("off", func(t *testing.T) {
		waf, err := NewHandle(rule, "", "")
		require.NoError(t, err)
		defer waf.Close()
		wafCtx := waf.NewContext()
		require.NotNil(t, wafCtx)
		defer wafCtx.Close()
		data := map[string]interface{}{
			"my.addr": map[string]interface{}{"key": "Arachni-sensitive-Arachni"},
		}
		matches, actions, err := wafCtx.Run(data, time.Second)
		require.NotNil(t, matches)
		require.Nil(t, actions)
		require.NoError(t, err)
		require.Contains(t, (string)(matches), "sensitive")
	})
}

func BenchmarkEncoder(b *testing.B) {
	rnd := rand.New(rand.NewSource(33))
	buf := make([]byte, 16384)
	n, err := rnd.Read(buf)
	fullstr := string(buf)
	for _, l := range []int{1024, 4096, 8192, 16384} {
		encoder := encoder{
			objectMaxDepth:   10,
			stringMaxSize:    1 * 1024 * 1024,
			containerMaxSize: 100,
		}
		b.Run(fmt.Sprintf("%d", l), func(b *testing.B) {
			str := fullstr[:l]
			slice := []string{str, str, str, str, str, str, str, str, str, str}
			data := map[string]interface{}{
				"k0": slice,
				"k1": slice,
				"k2": slice,
				"k3": slice,
				"k4": slice,
				"k5": slice,
				"k6": slice,
				"k7": slice,
				"k8": slice,
				"k9": slice,
			}
			if err != nil || n != len(buf) {
				b.Fatal(err)
			}
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				_, err := encoder.Encode(data)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
