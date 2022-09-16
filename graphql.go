// Package graphql provides a low level GraphQL client.
//
//  // create a client (safe to share across requests)
//  client := graphql.NewClient("https://machinebox.io/graphql")
//
//  // make a request
//  req := graphql.NewRequest(`
//      query ($key: String!) {
//          items (id:$key) {
//              field1
//              field2
//              field3
//          }
//      }
//  `)
//
//  // set any variables
//  req.Var("key", "value")
//
//  // run it and capture the response
//  var respData ResponseStruct
//  if err := client.Run(ctx, req, &respData); err != nil {
//      log.Fatal(err)
//  }
//
// Specify client
//
// To specify your own http.Client, use the WithHTTPClient option:
//  httpclient := &http.Client{}
//  client := graphql.NewClient("https://machinebox.io/graphql", graphql.WithHTTPClient(httpclient))
package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
)

type BuildHeaderFunc func(reqBody string) http.Header

// Client is a client for interacting with a GraphQL API.
type Client struct {
	endpoint         string
	httpClient       *http.Client
	useMultipartForm bool

	// closeReq will close the request body immediately allowing for reuse of client
	closeReq bool

	// Log is called with various debug information.
	// To log to standard out, use:
	//  client.Log = func(s string) { log.Println(s) }
	Log func(s string)

	buildHeaderFunc BuildHeaderFunc
}

// NewClient makes a new Client capable of making GraphQL requests.
func NewClient(endpoint string, opts ...ClientOption) *Client {
	c := &Client{
		endpoint: endpoint,
		Log:      func(string) {},
	}
	for _, optionFunc := range opts {
		optionFunc(c)
	}
	if c.httpClient == nil {
		c.httpClient = http.DefaultClient
	}
	return c
}

func (c *Client) logf(format string, args ...interface{}) {
	c.Log(fmt.Sprintf(format, args...))
}

// Run executes the query and unmarshals the response from the data field
// into the response object.
// Pass in a nil response object to skip response parsing.
// If the request fails or the server returns an error, the first error
// will be returned.
func (c *Client) Run(ctx context.Context, req *Request, resp interface{}) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if len(req.files) > 0 && !c.useMultipartForm {
		return errors.New("cannot send files with PostFields option")
	}
	if c.useMultipartForm {
		return c.runWithPostFields(ctx, req, resp)
	}
	return c.runWithJSON(ctx, req, resp)
}

func (c *Client) runWithJSON(ctx context.Context, req *Request, resp interface{}) error {
	var requestBody bytes.Buffer
	requestBodyObj := struct {
		Query     string                 `json:"query"`
		Variables map[string]interface{} `json:"variables"`
	}{
		Query:     req.q,
		Variables: req.vars,
	}
	if err := json.NewEncoder(&requestBody).Encode(requestBodyObj); err != nil {
		return errors.Wrap(err, "encode body")
	}
	c.logf(">> variables: %v", req.vars)
	c.logf(">> query: %s", req.q)
	gr := &graphResponse{
		Data: resp,
	}
	r, err := http.NewRequest(http.MethodPost, c.endpoint, &requestBody)
	if err != nil {
		return err
	}
	r.Close = c.closeReq
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	r.Header.Set("Accept", "application/json; charset=utf-8")
	for key, values := range req.Header {
		for _, value := range values {
			r.Header.Add(key, value)
		}
	}
	c.logf(">> headers: %v", r.Header)
	r = r.WithContext(ctx)
	res, err := c.httpClient.Do(r)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, res.Body); err != nil {
		return errors.Wrap(err, "reading body")
	}
	c.logf("<< %s", buf.String())
	if err := json.NewDecoder(&buf).Decode(&gr); err != nil {
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("graphql: server returned a non-200 status code: %v", res.StatusCode)
		}
		return errors.Wrap(err, "decoding response")
	}
	if len(gr.Errors) > 0 {
		// return first error
		return gr.Errors[0]
	}
	return nil
}

var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"")

func escapeQuotes(s string) string {
	return quoteEscaper.Replace(s)
}

func createFormFile(w *multipart.Writer, fieldname, filename string, contentType string) (io.Writer, error) {
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="%s"; filename="%s"`,
			escapeQuotes(fieldname), escapeQuotes(filename)))
	if contentType != "" {
		h.Set("Content-Type", contentType)
	} else {
		h.Set("Content-Type", "application/octet-stream")
	}
	return w.CreatePart(h)
}

func (c *Client) runWithPostFields(ctx context.Context, req *Request, resp interface{}) error {
	var requestBody bytes.Buffer
	requestBodyObj := struct {
		Query     string                 `json:"query"`
		Variables map[string]interface{} `json:"variables"`
	}{
		Query:     req.q,
		Variables: req.vars,
	}
	reqStr, err := json.Marshal(&requestBodyObj)
	if err != nil {
		return errors.Wrap(err, "json marshal failed")
	}
	writer := multipart.NewWriter(&requestBody)
	if err := writer.WriteField("operations", string(reqStr)); err != nil {
		return errors.Wrap(err, "write query field")
	}
	if len(req.files) > 0 {
		mapPart := make(map[string][]string)
		for _, f := range req.files {
			mapPart[f.PartName] = []string{f.Field}
		}
		mapPartStr, err := json.Marshal(mapPart)
		if err != nil {
			return errors.Wrap(err, "json marshal failed")
		}
		if err := writer.WriteField("map", string(mapPartStr)); err != nil {
			return errors.Wrap(err, "write query field")
		}
	}
	for i := range req.files {
		part, err := createFormFile(writer, req.files[i].PartName, req.files[i].Name, req.files[i].ContentType)
		if err != nil {
			return errors.Wrap(err, "create form file")
		}
		if _, err := io.Copy(part, req.files[i].R); err != nil {
			return errors.Wrap(err, "preparing file")
		}
	}
	if err := writer.Close(); err != nil {
		return errors.Wrap(err, "close writer")
	}
	c.logf(">> files: %d", len(req.files))
	c.logf(">> query: %s", req.q)
	gr := &graphResponse{
		Data: resp,
	}
	r, err := http.NewRequest(http.MethodPost, c.endpoint, &requestBody)
	if err != nil {
		return err
	}
	r.Close = c.closeReq
	r.Header.Set("Content-Type", writer.FormDataContentType())
	r.Header.Set("Accept", "application/json; charset=utf-8")
	for key, values := range req.Header {
		for _, value := range values {
			r.Header.Add(key, value)
		}
	}
	if c.buildHeaderFunc != nil {
		for key, values := range c.buildHeaderFunc(requestBody.String()) {
			for _, value := range values {
				r.Header.Add(key, value)
			}
		}

	}
	c.logf(">> headers: %v", r.Header)
	r = r.WithContext(ctx)
	res, err := c.httpClient.Do(r)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, res.Body); err != nil {
		return errors.Wrap(err, "reading body")
	}
	c.logf("<< %s", buf.String())
	if err := json.NewDecoder(&buf).Decode(&gr); err != nil {
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("graphql: server returned a non-200 status code: %v", res.StatusCode)
		}
		return errors.Wrap(err, "decoding response")
	}
	if len(gr.Errors) > 0 {
		// return first error
		return gr.Errors[0]
	}
	return nil
}

// WithHTTPClient specifies the underlying http.Client to use when
// making requests.
//  NewClient(endpoint, WithHTTPClient(specificHTTPClient))
func WithHTTPClient(httpclient *http.Client) ClientOption {
	return func(client *Client) {
		client.httpClient = httpclient
	}
}

func WithBuildHeaderFunc(f BuildHeaderFunc) ClientOption {
	return func(client *Client) {
		client.buildHeaderFunc = f
	}
}

// UseMultipartForm uses multipart/form-data and activates support for
// files.
func UseMultipartForm() ClientOption {
	return func(client *Client) {
		client.useMultipartForm = true
	}
}

//ImmediatelyCloseReqBody will close the req body immediately after each request body is ready
func ImmediatelyCloseReqBody() ClientOption {
	return func(client *Client) {
		client.closeReq = true
	}
}

// ClientOption are functions that are passed into NewClient to
// modify the behaviour of the Client.
type ClientOption func(*Client)

type graphErr struct {
	Message string
}

func (e graphErr) Error() string {
	return "graphql: " + e.Message
}

type graphResponse struct {
	Data   interface{}
	Errors []graphErr
}

// Request is a GraphQL request.
type Request struct {
	q     string
	vars  map[string]interface{}
	files []File

	// Header represent any request headers that will be set
	// when the request is made.
	Header http.Header
}

// NewRequest makes a new Request with the specified string.
func NewRequest(q string) *Request {
	req := &Request{
		q:      q,
		Header: make(map[string][]string),
	}
	return req
}

// Var sets a variable.
func (req *Request) Var(key string, value interface{}) {
	if req.vars == nil {
		req.vars = make(map[string]interface{})
	}
	req.vars[key] = value
}

// Vars gets the variables for this Request.
func (req *Request) Vars() map[string]interface{} {
	return req.vars
}

// Files gets the files in this request.
func (req *Request) Files() []File {
	return req.files
}

// Query gets the query string of this request.
func (req *Request) Query() string {
	return req.q
}

// File sets a file to upload.
// Files are only supported with a Client that was created with
// the UseMultipartForm option.
func (req *Request) File(fieldname, filename string, r io.Reader, contentType string) {
	oriLen := len(req.files)
	req.files = append(req.files, File{
		Field:       fieldname,
		PartName:    fmt.Sprintf("%d", oriLen),
		Name:        filename,
		ContentType: contentType,
		R:           r,
	})
}

// File represents a file to upload.
type File struct {
	Field       string
	PartName    string
	Name        string
	ContentType string
	R           io.Reader
}

type SubscriptionClient struct {
	subWebsocket *websocket.Conn
	subBuffer    chan subscriptionMessage
	subWait      sync.WaitGroup
	subs         sync.Map
	subIdGen     int
}

type subscriptionMessageType string

const (
	gql_connection_init       subscriptionMessageType = "connection_init"
	gql_start                                         = "start"
	gql_stop                                          = "stop"
	gql_connection_ack                                = "connection_ack"
	gql_connection_terminate                          = "connection_terminate"
	gql_connection_error                              = "connection_error"
	gql_data                                          = "data"
	gql_error                                         = "error"
	gql_complete                                      = "GQL_COMPLETE"
	gql_connection_keep_alive                         = "ka"
)

type subscriptionMessage struct {
	Payload *json.RawMessage        `json:"payload,omitempty"`
	Id      *string                 `json:"id,omitempty"`
	Type    subscriptionMessageType `json:"type"`
}

func (c *Client) SubscriptionClient(ctx context.Context, header http.Header) (*SubscriptionClient, error) {
	dialer := websocket.DefaultDialer
	header.Set("Sec-WebSocket-Protocol", "graphql-ws")
	header.Set("Content-Type", "application/json")

	conn, _, err := dialer.DialContext(ctx, strings.Replace(c.endpoint, "http", "ws", 1), header)

	if err != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, err
	}
	subClient := &SubscriptionClient{
		subWebsocket: conn,
		subBuffer:    make(chan subscriptionMessage),
	}

	var msg subscriptionMessage

	msg.Type = gql_connection_init
	emptyPayload := json.RawMessage("{}")
	msg.Payload = &emptyPayload
	err = conn.WriteJSON(msg)
	if err != nil {
		return nil, err
	}

	err = conn.ReadJSON(&msg)
	if err != nil {
		return nil, err
	}

	if msg.Type != gql_connection_ack {
		conn.Close()
		if msg.Type == gql_connection_error {
			errJ, _ := json.Marshal(*msg.Payload)
			return nil, errors.New(string(errJ))
		} else {
			return nil, errors.New("server-did-not-acknowledge")
		}
	}

	go subClient.subWork()
	return subClient, nil
}

func (c *SubscriptionClient) Close() error {
	if c.subWebsocket == nil {
		return nil
	}
	err := c.subWebsocket.WriteJSON(subscriptionMessage{Type: gql_connection_terminate})
	if err != nil {
		return err
	}

	c.subWait.Wait()
	err = c.subWebsocket.Close()
	if err != nil {
		return err
	}
	return nil
}

type SubscriptionPayload struct {
	Data  *json.RawMessage
	Error *json.RawMessage
}

type Subscription chan SubscriptionPayload

func (c *SubscriptionClient) subWork() {
	c.subWait.Add(1)
	defer c.subWait.Done()
	defer c.subs.Range(func(_, sub interface{}) bool {
		close(sub.(Subscription))
		return true
	})

	for {
		var msg subscriptionMessage
		err := c.subWebsocket.ReadJSON(&msg)

		if err != nil {
			if err == io.ErrUnexpectedEOF || err == io.EOF {
				//close every subscription
				return
			}
			if strings.HasSuffix(err.Error(), io.ErrUnexpectedEOF.Error()) {
				return
			}

			log.Fatalf("Error reading from subscription websocket : %s", err)
			return
		}

		switch msg.Type {
		case gql_error:
			id := *msg.Id
			ch, _ := c.subs.Load(id)
			ch.(Subscription) <- SubscriptionPayload{Error: msg.Payload}
		case gql_data:
			id := *msg.Id
			ch, _ := c.subs.Load(id)
			ch.(Subscription) <- SubscriptionPayload{Data: msg.Payload}
		case gql_complete:
			id := *msg.Id
			ch, _ := c.subs.Load(id)
			close(ch.(Subscription))
			c.subs.Delete(id)

		case gql_connection_keep_alive: //ignore...
		}
	}
}

func (c *SubscriptionClient) Subscribe(req *Request) (Subscription, error) {

	var requestBody bytes.Buffer
	requestBodyObj := struct {
		Query     string                 `json:"query"`
		Variables map[string]interface{} `json:"variables"`
	}{
		Query:     req.q,
		Variables: req.vars,
	}
	if err := json.NewEncoder(&requestBody).Encode(requestBodyObj); err != nil {
		return nil, errors.Wrap(err, "encode body")
	}
	id := strconv.Itoa(c.subIdGen)
	c.subIdGen++

	payload := json.RawMessage(requestBody.Bytes())
	sReq := subscriptionMessage{
		Payload: &payload,
		Id:      &id,
		Type:    gql_start,
	}

	subChan := make(Subscription)
	c.subs.Store(id, subChan)
	err := c.subWebsocket.WriteJSON(sReq)
	if err != nil {
		return nil, err
	}

	return subChan, nil
}

func (c *SubscriptionClient) Unsubscribe(sub Subscription) {
	c.subs.Range(func(key interface{}, value interface{}) bool {
		if value == sub {
			id := key.(string)
			_ = c.subWebsocket.WriteJSON(subscriptionMessage{Id: &id, Type: gql_stop})
			return false
		}
		return true
	})
}
