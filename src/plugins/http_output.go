package plugins

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hexdecteam/easegateway-types/pipelines"
	"github.com/hexdecteam/easegateway-types/plugins"
	"github.com/hexdecteam/easegateway-types/task"

	"common"
	"logger"
)

type httpOutputConfig struct {
	CommonConfig
	URLPattern               string            `json:"url_pattern"`
	HeaderPatterns           map[string]string `json:"header_patterns"`
	Close                    bool              `json:"close_body_after_pipeline"`
	RequestBodyBufferPattern string            `json:"request_body_buffer_pattern"`
	Method                   string            `json:"method"`
	ExpectedResponseCode     string            `json:"expected_response_code"`
	TimeoutSec               uint16            `json:"timeout_sec"` // up to 65535, zero means no timeout
	CertFile                 string            `json:"cert_file"`
	KeyFile                  string            `json:"key_file"`
	CAFile                   string            `json:"ca_file"`
	Insecure                 bool              `json:"insecure_tls"`

	RequestBodyIOKey  string `json:"request_body_io_key"`
	ResponseCodeKey   string `json:"response_code_key"`
	ResponseBodyIOKey string `json:"response_body_io_key"`

	expectedResponseCode *regexp.Regexp

	cert   *tls.Certificate
	caCert []byte
}

func HTTPOutputConfigConstructor() plugins.Config {
	return &httpOutputConfig{
		TimeoutSec: 120,
		Close:      true,
		ExpectedResponseCode: ".*",
	}
}

func (c *httpOutputConfig) Prepare(pipelineNames []string) error {
	err := c.CommonConfig.Prepare(pipelineNames)
	if err != nil {
		return err
	}

	ts := strings.TrimSpace
	c.URLPattern = ts(c.URLPattern)
	c.RequestBodyIOKey = ts(c.RequestBodyIOKey)
	c.Method = ts(c.Method)
	c.ExpectedResponseCode = ts(c.ExpectedResponseCode)
	c.CertFile = ts(c.CertFile)
	c.KeyFile = ts(c.KeyFile)
	c.CAFile = ts(c.CAFile)
	c.ResponseCodeKey = ts(c.ResponseCodeKey)
	c.ResponseBodyIOKey = ts(c.ResponseBodyIOKey)

	uri, err := url.ParseRequestURI(c.URLPattern)
	if err != nil || !uri.IsAbs() || uri.Hostname() == "" ||
		!common.StrInSlice(uri.Scheme, []string{"http", "https"}) {

		return fmt.Errorf("invalid url")
	}

	_, err = common.ScanTokens(c.URLPattern, false, nil)
	if err != nil {
		return fmt.Errorf("invalid url pattern")
	}

	for name, value := range c.HeaderPatterns {
		if len(ts(name)) == 0 {
			return fmt.Errorf("invalid header name")
		}

		_, err := common.ScanTokens(name, false, nil)
		if err != nil {
			return fmt.Errorf("invalid header name pattern")
		}

		_, err = common.ScanTokens(value, false, nil)
		if err != nil {
			return fmt.Errorf("invalid header value pattern")
		}
	}

	_, ok := supportedMethods[c.Method]
	if !ok {
		return fmt.Errorf("invalid http method")
	}

	c.expectedResponseCode, err = regexp.Compile(c.ExpectedResponseCode)
	if err != nil {
		return fmt.Errorf("invalid expected response code: %v", err)
	}

	if c.TimeoutSec == 0 {
		logger.Warnf("[ZERO timeout has been applied, no request could be cancelled by timeout!]")
	}

	_, err = common.ScanTokens(c.RequestBodyBufferPattern, false, nil)
	if err != nil {
		return fmt.Errorf("invalid body buffer pattern")
	}

	if len(c.CertFile) != 0 && len(c.KeyFile) != 0 {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return fmt.Errorf("invalid PEM eoncoded certificate and/or preivate key file(s)")
		}
		c.cert = &cert
	}

	if len(c.CAFile) != 0 {
		c.caCert, err = ioutil.ReadFile(c.CAFile)
		if err != nil {
			return fmt.Errorf("invalid PEM eoncoded CA certificate file")
		}
	}

	return nil
}

////

type httpOutput struct {
	conf   *httpOutputConfig
	client *http.Client
}

func HTTPOutputConstructor(conf plugins.Config) (plugins.Plugin, error) {
	c, ok := conf.(*httpOutputConfig)
	if !ok {
		return nil, fmt.Errorf("config type want *httpOutputConfig got %T", conf)
	}

	tlsConfig := new(tls.Config)
	tlsConfig.InsecureSkipVerify = c.Insecure

	h := &httpOutput{
		conf: c,
		client: &http.Client{
			Timeout:   time.Duration(c.TimeoutSec) * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsConfig},
		},
	}

	if c.cert != nil {
		tlsConfig.Certificates = []tls.Certificate{*c.cert}
		tlsConfig.BuildNameToCertificate()
	}

	if c.caCert != nil {
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(c.caCert)
		tlsConfig.RootCAs = caCertPool
	}

	return h, nil
}

func (h *httpOutput) Prepare(ctx pipelines.PipelineContext) {
	// Nothing to do.
}

func (h *httpOutput) send(t task.Task, req *http.Request) (*http.Response, error) {
	r := make(chan *http.Response)
	e := make(chan error)

	defer close(r)
	defer close(e)

	cancelCtx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(cancelCtx)

	go func() {
		defer func() {
			// channel e and r can be closed first before return by existing send()
			// caused by task cancellation, the result or error of Do() can be ignored safely.
			recover()
		}()

		resp, err := h.client.Do(req)
		if err != nil {
			e <- err
		}
		r <- resp
	}()

	select {
	case resp := <-r:
		return resp, nil
	case err := <-e:
		t.SetError(err, task.ResultServiceUnavailable)
		return nil, err
	case <-t.Cancel():
		cancel()
		err := fmt.Errorf("task is cancelled by %s", t.CancelCause())
		t.SetError(err, task.ResultTaskCancelled)
		return nil, err
	}
}

func (h *httpOutput) Run(ctx pipelines.PipelineContext, t task.Task) (task.Task, error) {
	// skip error check safely due to we ensured it in Prepare()
	link, _ := ReplaceTokensInPattern(t, h.conf.URLPattern)

	var length int64
	var reader io.Reader
	if len(h.conf.RequestBodyIOKey) != 0 {
		inputValue := t.Value(h.conf.RequestBodyIOKey)
		input, ok := inputValue.(io.Reader)
		if !ok {
			t.SetError(fmt.Errorf("input %s got wrong value: %#v", h.conf.RequestBodyIOKey, inputValue),
				task.ResultMissingInput)
			return t, nil
		}

		// optimization and defensive for http proxy case
		lenValue := t.Value("HTTP_CONTENT_LENGTH")
		clen, ok := lenValue.(string)
		if ok {
			var err error
			length, err = strconv.ParseInt(clen, 10, 64)
			if err == nil && length >= 0 {
				reader = io.LimitReader(input, length)
			} else {
				reader = input
			}
		} else {
			// Request.ContentLength of 0 means either actually 0 or unknown
			reader = input
		}
	} else {
		// skip error check safely due to we ensured it in Prepare()
		body, _ := ReplaceTokensInPattern(t, h.conf.RequestBodyBufferPattern)
		reader = bytes.NewBuffer([]byte(body))
		length = int64(len(body))
	}

	req, err := http.NewRequest(h.conf.Method, link, reader)
	if err != nil {
		t.SetError(err, task.ResultInternalServerError)
		return t, nil
	}
	req.ContentLength = length

	i := 0
	for name, value := range h.conf.HeaderPatterns {
		// skip error check safely due to we ensured it in Prepare()
		name1, _ := ReplaceTokensInPattern(t, name)
		value1, _ := ReplaceTokensInPattern(t, value)
		req.Header.Set(name1, value1)
		i++
	}
	req.Header.Set("User-Agent", "EaseGateway")

	resp, err := h.send(t, req)
	if err != nil {
		return t, nil
	}

	responseCode := task.ToString(resp.StatusCode, option.PluginIODataFormatLengthLimit)
	if !h.conf.expectedResponseCode.MatchString(responseCode) {
		err = fmt.Errorf("response code: %d doesn't match with expected : %s", resp.StatusCode, h.conf.ExpectedResponseCode)
		t.SetError(err, task.ResultInternalServerError)
		return t, nil
	}

	if len(h.conf.ResponseCodeKey) != 0 {
		t, err = task.WithValue(t, h.conf.ResponseCodeKey, resp.StatusCode)
		if err != nil {
			t.SetError(err, task.ResultInternalServerError)
			return t, nil
		}
	}

	if len(h.conf.ResponseBodyIOKey) != 0 {
		t, err = task.WithValue(t, h.conf.ResponseBodyIOKey, resp.Body)
		if err != nil {
			t.SetError(err, task.ResultInternalServerError)
			return t, nil
		}
	}

	if h.conf.Close {
		closeHTTPOutputResponseBody := func(t1 task.Task, _ task.TaskStatus) {
			t1.DeleteFinishedCallback(fmt.Sprintf("%s-closeHTTPOutputResponseBody", h.Name()))

			resp.Body.Close()
		}

		t.AddFinishedCallback(fmt.Sprintf("%s-closeHTTPOutputResponseBody", h.Name()),
			closeHTTPOutputResponseBody)
	}

	return t, nil
}

func (h *httpOutput) Name() string {
	return h.conf.PluginName()
}

func (h *httpOutput) Close() {
	// Nothing to do.
}
