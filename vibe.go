/*
 * Vibe Server
 * http://vibe-project.github.io/projects/vibe-protocol/
 *
 * Copyright 2014 The Vibe Project
 * Licensed under the Apache License, Version 2.0
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Go port (c) 2014 fromkeith
 * Dual licensed under BSD
 */

package vibe

import (
    "time"
    "net/http"
    "strings"
    "code.google.com/p/go-uuid/uuid"
    "encoding/json"
    "io"
    "bytes"
    "net/url"
    "strconv"
    "net"
    "fmt"
    "errors"
    "sync"
    "github.com/gorilla/websocket"
)





type TransportType string

const (
    Ws                  TransportType = "ws"
    Sse                 TransportType = "sse"
    StreamXhr           TransportType = "streamxhr"
    StreamXdr           TransportType = "streamxdr"
    StreamIframe        TransportType = "streamiframe"
    LongpollAjax        TransportType = "longpollajax"
    LongpollXdr         TransportType = "longpollxdr"
    LongpollJsonp       TransportType = "longpolljsonp"
)

var (
    AllTransports = []TransportType{
        Ws,
        Sse,
        StreamXhr,
        StreamXdr,
        StreamIframe,
        LongpollAjax,
        LongpollXdr,
        LongpollJsonp,
    }
)

// A listener for the whole server
type ServerListener interface {
    // Socket gets called when a new socket has been opened. The request is
    // the one opening the socket. This can be used to associate authentication
    // data with an open socket.
    Socket(s *VibeSocket, req*http.Request)

    // authorizes the request. Return false to deny the request.
    // a vibesocket will be provided if it is an socket being manipulated
    Auth(req *http.Request, s*VibeSocket) bool

    // used for logging
    Log(format string, args... interface{})
}

type SocketListener interface {
    // An error occured
    Error(err error)
    // Socket was closed
    Close()
    // A new message arrived from the client
    Message(messageType string, data interface{})
    // The client is requiring a reply to this message
    ReplyMessage(messageType string, data interface{}, replyWith func (resolve bool, value interface{}))
}

// Root instance of our Server
type Server struct {
    transports      []TransportType
    heartbeat       int64
    // The listener to get events
    Listener        ServerListener
    sockets         map[string]*VibeSocket
    socketMapLock   sync.RWMutex
}

// Represents a single client connect to the Server
type VibeSocket struct {
    transport       transportInt
    // The Id of this connection
    Id              string
    // The listener to listen to socket events to.
    Listener        SocketListener
    // An auto-increment id for event. In case of long polling, these ids
    // are echoed back as a query string to the URL in GET. To avoid `414
    // Request-URI Too Long` error, though it is not that important, it
    // would be better to use small sized id. Moreover, it should be unique
    // among events to be sent to the client and has nothing to do with one
    // the client sent.
    eventId         int64
    // A map for reply callbacks for `reply` extension.
    callbacks       map[string]SocketCallback
    // our parent Server
    Server          *Server
    heartbeatTimer  *time.Timer

}

// When we get a reply, this will called
// if resolve is false then an exeception was reported by the client
// value is the data that was sent back to us
type SocketCallback func(resolve bool, value interface{})

// Creates a new server
//  transports - a set of supported transports to be used by a client. If nil then all used.
//  heartbeat - interval in milliseconds for heartbeat. If 0 then 20 seconds is used.
func NewServer(transports []TransportType, heartbeat time.Duration) *Server {
    s := new(Server)
    s.heartbeat = int64(heartbeat)
    s.transports = transports
    if s.heartbeat == 0 {
        s.heartbeat = int64((20 * time.Second) / time.Millisecond)
    }
    if len(s.transports) == 0 {
        s.transports = AllTransports
    }
    s.sockets = make(map[string]*VibeSocket)
    return s
}


type handshakeResult struct {
    Id              string                  `json:"id"`
    Transports      []TransportType         `json:"transports"`
    Heartbeat       int64                   `json:"heartbeat"`
    testHeartbeat   int64                   `json:"_heartbeat"`
}

func (serv *Server) supportsTransport(what string) bool {
    whatTrans := TransportType(what)
    for i := range serv.transports {
        if serv.transports[i] == whatTrans {
            return true
        }
    }
    return false
}

// sets the transpotrs we are using
func (serv *Server) SetTransports(t []TransportType) {
    serv.transports = t
    if len(serv.transports) == 0 {
        serv.transports = AllTransports
    }
}

// sets the heartbeat timeout
func (serv *Server) SetHeartbeat(t time.Duration) {
    serv.heartbeat = int64(t / time.Millisecond)
    if serv.heartbeat == 0 {
        serv.heartbeat = int64((20 * time.Second) / time.Millisecond)
    }
}

func (serv *Server) IsSocketAlive(id string) bool {
    _, ok := serv.getSocket(id)
    return ok
}

func (serv *Server) getSocket(id string) (*VibeSocket, bool) {
    serv.socketMapLock.RLock()
    defer serv.socketMapLock.RUnlock()
    v, ok := serv.sockets[id]
    return v, ok
}

func (serv *Server) setSocket(id string, v *VibeSocket) {
    serv.socketMapLock.Lock()
    defer serv.socketMapLock.Unlock()
    serv.sockets[id] = v
}

func (serv *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
    defer func () {
        rec := recover()
        if rec != nil {
            serv.Listener.Log("Paniced: %v", rec)
            http.Error(w, "ServerError", 500)
        }
    }()
    // Any request must not be cached.
    w.Header().Set("cache-control", "no-cache, no-store, must-revalidate")
    w.Header().Set("pragma", "no-cache")
    w.Header().Set("expires", "0")
    // `streamxdr` or `longpollxdr` transportInt requires CORS headers even in
    // same-origin connection.
    origin := req.Header.Get("origin")
    if origin == "" {
        origin = "*"
    }
    w.Header().Set("access-control-allow-origin",  origin)
    w.Header().Set("access-control-allow-credentials", "true")

    params := req.URL.Query()

    serv.Listener.Log("vibe.Server.ServeHttp: %s : %s", req.Method, req.URL.RequestURI())

    defer req.Body.Close()

    switch strings.ToUpper(req.Method) {
        case "GET":
            switch params.Get(`when`) {
                // Negotiates the protocol. Information to connect to the
                // server are passed to the client.
                case "handshake":
                    if !serv.Listener.Auth(req, nil) {
                        http.Error(w, "Not Authorized", 403)
                        return
                    }
                    // A result of handshaking is a JSON containing that
                    // information.
                    res := handshakeResult{
                        Id: uuid.New(),
                        Transports: serv.transports,
                        Heartbeat: int64(serv.heartbeat),
                        testHeartbeat: int64((5 * time.Second) / time.Millisecond),
                    }
                    respBytes, err := json.Marshal(res)
                    if err != nil {
                        panic(err)
                    }
                    // An old client like browsers not implementing CORS may have to
                    // use JSONP because this request would be cross origin. If that
                    // is the case, `callback` parameter will be passed for JSONP.
                    if params.Get(`callback`) != "" {
                        respBytes = []byte(params.Get(`callback`) + "(" + string(respBytes) + ");")
                    }
                    w.WriteHeader(200)
                    io.Copy(w, bytes.NewReader(respBytes))
                    break
                // Open a new socket establishing required transportInt and fires the
                // `socket` event. `transportInt` param is an id of transportInt the
                // client uses.
                case "open":
                    if !serv.Listener.Auth(req, nil) {
                        http.Error(w, "Not Authorized", 403)
                        return
                    }
                    // If the server doesn't support the required transportInt,
                    // responds with `501 Not Implemented`. However, it's 
                    // unlikely to happen.
                    if !serv.supportsTransport(params.Get("transport")) {
                        w.WriteHeader(501)
                        return
                    }
                    s := serv.socket(serv, params, serv.createTransport(w, req, params))
                    //s.Uri = req.URL
                    if serv.Listener != nil {
                        serv.Listener.Socket(s, req)
                    }
                    s.transport.Wait()
                    break
                // Inject a new exchange of request and response to the long polling
                // transportInt of the socket whose id is `id` param. In long polling,
                // a pseudo-connection consisting of disposable exchanges pretends
                // to be a persistent connection.
                case "poll":
                    if s, ok := serv.getSocket(params.Get("id")); ok {
                        if !serv.Listener.Auth(req, s) {
                            http.Error(w, "Not Authorized", 403)
                            return
                        }
                        s.transport.Refresh(w, req, params)
                        s.transport.Wait()
                    } else {
                        // If there is no corresponding socket, responds with `500
                        // Internal Server Error`.
                        w.WriteHeader(500)
                    }
                    break
                // It means the client considers the socket whose id is `id` param
                // as closed so abort the socket if the server couldn't detect it
                // for some reason.
                case "abort":
                    if s, ok := serv.getSocket(params.Get("id")); ok {
                        if !serv.Listener.Auth(req, s) {
                            http.Error(w, "Not Authorized", 403)
                            return
                        }
                        s.Close()
                    }
                    // In case of browser, it is performed by script tag so set
                    // content-type header to `text/javascript` to avoid warning.
                    // It's just a warning and not serious.
                    w.Header().Set("content-type", "text/javascript; charset=utf-8")
                    w.WriteHeader(200)
                    break
                // If the given `when` param is unsupported, responds with `501 Not
                // Implemented`.
                default:
                    w.WriteHeader(501)
            }
            break
        // `POST` method is used to supply HTTP transportInt with message as a
        // channel for the client to push something to the server.
        case "POST":
            s, ok := serv.getSocket(params.Get("id"))
            if !ok {
                // If the specified socket is not found,
                // responds with `500 Internal Server Error`.
                w.WriteHeader(500)
                return
            }
            if !serv.Listener.Auth(req, s) {
                http.Error(w, "Not Authorized", 403)
                return
            }
            // Reads body to retrieve message. Only text data is allowed now.
            buf := bytes.Buffer{}
            if _, err := io.Copy(&buf, req.Body); err != nil {
                w.WriteHeader(500)
                return
            }
            text := strings.TrimPrefix(buf.String(), "data=")
            // Fires a message event to the socket's transportInt
            // whose id is `id` param with that text message.
            s.OnTransportMessage(text)
            w.WriteHeader(200)
            break
        // If the method is neither `GET` nor `POST`, responds with `405 Method
        // Not Allowed`.
        default:
            w.WriteHeader(405)
            break
    }
}

func (serv *Server) createTransport(w http.ResponseWriter, req *http.Request, params url.Values) transportInt {
    switch params.Get("transport") {
    case "streamxhr":
        fallthrough
    case "streamxdr":
        fallthrough
    case "streamiframe":
        fallthrough
    case "sse":
        return newSseTransport(w, req, params)
    case "longpollxdr":
        fallthrough
    case "longpolljsonp":
        fallthrough
    case "longpollajax":
        return newLongPollAjax(w, req, params)
    case "ws":
        return newWebsocketTransport(w, req, params)
    default:
        w.WriteHeader(401)
        req.Body.Close()
        return nil
    }
}


var upgrader = websocket.Upgrader{
    ReadBufferSize:  1024,
    WriteBufferSize: 1024,
    CheckOrigin: func (r *http.Request) bool {return true},
}

// TODO: websockets upgrade
type websocketTransport struct {
    conn            *websocket.Conn
    writeQueue      chan []byte
    closedLock      sync.Mutex
    closed          bool
    listener        transportListener
}

func newWebsocketTransport(w http.ResponseWriter, req *http.Request, params url.Values) *websocketTransport {
    conn, err := upgrader.Upgrade(w, req, nil)
    if err != nil {
        panic(err)
    }
    ws := &websocketTransport{
        conn: conn,
        writeQueue: make(chan []byte),
    }
    go ws.Read()
    return ws
}

func (w *websocketTransport) Send(data []byte) {
    w.closedLock.Lock()
    defer w.closedLock.Unlock()
    if w.closed {
        return
    }
    w.writeQueue <- data
}
func (w *websocketTransport) Close() {
    w.closedLock.Lock()
    defer w.closedLock.Unlock()
    if w.closed {
        return
    }
    w.closed = true
    close(w.writeQueue)
    w.conn.Close()
    w.listener.OnTransportClose()
}
func (w *websocketTransport) SetListener(l transportListener) {
    w.listener = l
}
func (w *websocketTransport) Refresh(wr http.ResponseWriter, req *http.Request, params url.Values) {
    panic("not supported")
}
func (w *websocketTransport) Read() {
    for {
        typ, p, err := w.conn.ReadMessage()
        if err != nil {
            // closed
            w.Close()
            return
        }
        if typ != websocket.TextMessage {
            fmt.Println("Message: ", typ, string(p))
            continue
        }
        w.listener.OnTransportMessage(string(p))
    }
}
func (w *websocketTransport) Wait() {
    for {
        msg, ok := <- w.writeQueue
        if !ok {
            return
        }
        err := w.conn.WriteMessage(websocket.TextMessage, msg)
        if err != nil {
            w.listener.OnTransportError(err)
        }
    }
}


// A socket is an interface to exchange event between the two endpoints and
// expected to be public for developers to create vibe application. The
// event is serialized to and deseriazlied from JSON specified in
// [ECMA-404](http://www.ecma-international.org/publications/files/ECMA-ST/ECMA-404.pdf).
func (serv *Server) socket(server *Server, params url.Values, t transportInt) *VibeSocket {
    socket := new(VibeSocket)
    socket.Id = params.Get("id")
    socket.transport = t
    socket.Server = server

    t.SetListener(socket)
    socket.setHeartbeatTimer()

    socket.callbacks = make(map[string]SocketCallback)

    serv.setSocket(socket.Id, socket)
    return socket
}


func (socket *VibeSocket) OnTransportError(err error) {
    if socket.Listener != nil {
        socket.Listener.Error(err)
    }
}
func (socket *VibeSocket) OnTransportClose() {
    if socket.Listener != nil {
        socket.Listener.Close()
    }
    socket.heartbeatTimer.Stop()
    delete(socket.Server.sockets, socket.Id)
}


// It should have the following properties:
// * `id: string`: an event identifier.
// * `type: string`: an event type.
// * `data: any`: an event data.
// 
// If the server implements `reply` extension, the following
// properties should be considered as well.
// * `reply: boolean`: true if this event requires the reply.
type Message struct {
    Id              string      `json:"id"`
    Type            string      `json:"type"`
    Data            interface{} `json:"data,omitempty"`
    Reply           *bool       `json:"reply,omitempty"`
    Exception       *bool       `json:"exception,omitempty"`
}

// default vibe client sends Id as number... thats annoying.
type messageWithNumberId struct {
    Id              int64     `json:"id"`
    Type            string      `json:"type"`
    Data            interface{} `json:"data,omitempty"`
    Reply           *bool       `json:"reply,omitempty"`
    Exception       *bool       `json:"exception,omitempty"`
}

func (socket *VibeSocket) OnTransportMessage(msg string) {
    // Converts JSON to an message object.
    var event Message
    err := json.Unmarshal([]byte(msg), &event)
    if err != nil {
        var eventNum messageWithNumberId
        if err2 := json.Unmarshal([]byte(msg), &eventNum); err2 != nil {
            socket.Listener.Error(err)
            return
        }
        event.Id = fmt.Sprintf("%d", eventNum.Id)
        event.Type = eventNum.Type
        event.Data = eventNum.Data
        event.Reply = eventNum.Reply
        event.Exception = eventNum.Exception
    }

    if event.Reply == nil || *event.Reply == false {
        if event.Type == "heartbeat" {
            socket.setHeartbeatTimer()
            go socket.Send("heartbeat", "", nil)
        } else if event.Type == "reply" {
            socket.reply(event)
        } else {
            socket.Listener.Message(event.Type, event.Data)
        }
    } else {
        // This is how to implement `reply` extension. An event handler for
        // the corresponding event will receive reply controller as 2nd
        // argument. It calls the client's resolved or rejected callback by
        // sending `reply` event.
        latch := false
        socket.Listener.ReplyMessage(event.Type, event.Data, func (resolve bool, value interface{}) {
            if latch {
                return
            }
            latch = true
            notResolve := !resolve
            socket.Send(`reply`, Message{
                Id: event.Id,
                Data: value,
                Exception: &notResolve,
            }, nil)
        })
    }
}

func (socket *VibeSocket) reply(event Message) {
    asB, _ := json.Marshal(event.Data)
    var replyTo Message
    json.Unmarshal(asB, &replyTo)
    for k, v := range socket.callbacks {
        if replyTo.Id == k {
            resolved := replyTo.Exception == nil || !*replyTo.Exception
            v(resolved, replyTo.Data)
        }
        delete (socket.callbacks, k)
    }
}


// Call to send a message to the client
// t - type of message
// data - the data we want to send to the client
// resolveRejectCallback - if we want a reply, it will be sent to this
func (socket *VibeSocket) Send(t string, data interface{}, resolveRejectCallback SocketCallback) error {
    var event Message
    //if m, ok := data.(Message); ok {
    //    event = m
    //}
    event.Id = strconv.FormatInt(socket.eventId, 10)
    socket.eventId ++
    event.Type = t
    event.Data = data

    if resolveRejectCallback != nil {
        tru := true
        event.Reply = &tru
        socket.callbacks[event.Id] = resolveRejectCallback
    }
    dataByte, err := json.Marshal(event)
    if err != nil {
        socket.Listener.Error(err)
        return err
    }
    socket.transport.Send(dataByte)
    return nil
}

// Close the connection
func (socket *VibeSocket) Close() {
    // transportInt will call us to close OnTransportClose
    socket.transport.Close()
}

// Sets a timer to close the socket after the heartbeat interval.
func (socket *VibeSocket) setHeartbeatTimer() {
    if socket.heartbeatTimer != nil {
        socket.heartbeatTimer.Stop()
    }
    socket.heartbeatTimer = time.AfterFunc(time.Duration(socket.Server.heartbeat) * time.Millisecond, func () {
        socket.Listener.Error(errors.New("Heartbeat"))
        socket.Close()
    })
}



type transportInt interface {
    Send(data []byte)
    Close()
    SetListener(transportListener)
    Refresh(w http.ResponseWriter, req *http.Request, params url.Values)
    Wait()
}

type transportListener interface {
    OnTransportError(error)
    OnTransportClose()
    OnTransportMessage(message string)
}

// TODO: websocket


// HTTP Streaming is the way that the client performs a HTTP persistent
// connection and watches changes in response text and the server prints chunk
// as data to the connection.
//
// `sse` stands for [Server-Sent Events](http://www.w3.org/TR/eventsource/)
// specified by W3C.
type sseTransport struct {
    conn            net.Conn
    connRW          http.Flusher//*bufio.ReadWriter
    chunkWriter     http.ResponseWriter //io.WriteCloser
    listener        transportListener
    writeQueue      chan []byte
    closed          bool
    closedLock      sync.Mutex
}

func newSseTransport(w http.ResponseWriter, req *http.Request, params url.Values) *sseTransport {
    /*hj, ok := w.(http.Hijacker)
    if !ok {
        panic("cannot hijack request!")
        return nil
    }*/
    sse := new(sseTransport)
    sse.writeQueue = make(chan []byte)
    text2KB := make([]byte, 2048)
    for i := range text2KB {
        text2KB[i] = ' '
    }

    // The content-type headers should be `text/event-stream` for `sse` and
    // `text/plain` for others. Also the response should be encoded in `utf-8`
    // format for `sse`.
    transportType := "event-stream"
    if params.Get("transport") != "sse" {
        transportType = "plain"
    }
    w.Header().Set("content-type", fmt.Sprintf("text/%s; charset=utf-8", transportType))
    w.Header().Set("Connection", "keep-alive")
    w.WriteHeader(200)

    fl, ok := w.(http.Flusher)
    if !ok {
        panic("cannot flush!")
    }
    fl.Flush()
    sse.connRW = fl
    sse.chunkWriter = w
    sse.closed = false


    // The padding is required, which makes the client-side transportInt be aware
    // of change of the response and the client-side socket fire open event.
    // It should be greater than 1KB, be composed of white space character and 
    // end with `\r`, `\n` or `\r\n`. It applies to `streamxdr`, `streamiframe`.
    io.Copy(sse.chunkWriter, strings.NewReader(string(text2KB) + "\n"))
    sse.connRW.Flush()
    return sse
}

func (sse * sseTransport) Refresh(w http.ResponseWriter, req *http.Request, params url.Values) {
    panic("sseTransport does not support 'refresh'")
}

func (sse * sseTransport) Send(data []byte) {
    sse.closedLock.Lock()
    defer sse.closedLock.Unlock()
    if sse.closed {
        return
    }
    sse.writeQueue <- data
}

// Ends the response. Accordingly, `onclose` will be executed and the
// `finish` event will be fired. Don't do that by yourself.
func (sse *sseTransport) Close() {
    sse.connRW.Flush()

    sse.closedLock.Lock()
    sse.closed = true
    close(sse.writeQueue)
    sse.closedLock.Unlock()

    sse.listener.OnTransportClose()

}

func (sse *sseTransport) SetListener(tl transportListener) {
    sse.listener = tl
}

func (sse *sseTransport) Wait() {
    for {
        msg, ok := <- sse.writeQueue
        if !ok {
            return
        }
        // The response text should be formatted in the [event stream
        // format](http://www.w3.org/TR/eventsource/#parsing-an-event-stream).
        // This is specified in `sse` spec but the rest also accept that format
        // for convenience. According to the format, data should be broken up by
        // `\r`, `\n`, or `\r\n` but because data is JSON, it's not needed. So
        // prepend 'data: ' and append `\n\n` to the data.
        fmt.Fprintf(sse.chunkWriter, "data: " + string(msg) + "\n\n")
        sse.connRW.Flush()
    }
}

type longpollAjax struct {
    // Whether the transportInt is aborted or not.
    aborted         bool
    // Whether data is written on the current response or not. if this is true,
    // then `closed` is also true but not vice versa.
    written         bool
    // A timer to prevent from being idle connection.
    closeTimer      *time.Timer
    // A queue containing events that the client couldn't receive.
    queue           []Message
    // if we are a longpolljsonp type
    isLongpollJsonP     bool
    callbackForLongPollJsonP    string
    whenParam           string
    contentType         string

    listener        transportListener
    doneClose       bool

    refreshLock     sync.Mutex

    // our connection
    curConnection       *longpollConnection

}

type longpollConnection struct {
    writeQueue              chan []byte
    flush                   http.Flusher
    chunkWriter             http.ResponseWriter
    closed                  bool
    closeLock               sync.Mutex
}

func newLongPollAjax(w http.ResponseWriter, req *http.Request, params url.Values) *longpollAjax {
    lp := new(longpollAjax)
    lp.queue = make([]Message, 0, 50)
    lp.whenParam = params.Get("when")

    lp.contentType = "javascript"
    lp.isLongpollJsonP = true
    lp.callbackForLongPollJsonP = params.Get("callback")
    if params.Get("transport") != "longpolljsonp" {
        lp.contentType = "plain"
        lp.isLongpollJsonP = false
    }

    lp.Refresh(w, req, params)
    return lp
}

func (lp *longpollAjax) SetListener(tl transportListener) {
    lp.listener = tl
}

func (lp *longpollAjax) Wait() {
    con := lp.curConnection
    isFirst := true
    autoClose := time.After(time.Second * 2)
    defer func () {
        if !isFirst {
            if !lp.isLongpollJsonP {
                fmt.Fprint(con.chunkWriter, "]")
            }
            con.flush.Flush()
        }
    }()
    for {
        var data []byte
        var ok bool
        select {
            case data, ok = <- con.writeQueue:
            case <- autoClose:
                con.closeLock.Lock()
                defer con.closeLock.Unlock()
                con.closed = true
                return
        }
        if !ok {
            return
        }
        comma := ","
        if isFirst {
            if !lp.isLongpollJsonP {
                fmt.Fprint(con.chunkWriter, "[")
            }
            isFirst = false
            comma = ""
        }
        var err error
        if lp.isLongpollJsonP {
            buf := bytes.Buffer{}
            buf.WriteString(lp.callbackForLongPollJsonP)
            buf.WriteString("(")
            dataM, _ := json.Marshal(string(data))
            buf.Write(dataM)
            buf.WriteString(");")
            _, err = fmt.Fprint(con.chunkWriter, buf.String())
        } else {
            _, err = fmt.Fprintf(con.chunkWriter, "%s%s", comma, string(data))
        }
        if err != nil {
            lp.listener.OnTransportError(err)
        }
        con.flush.Flush()
    }
}

func (lp *longpollAjax) Refresh(w http.ResponseWriter, req *http.Request, params url.Values) {
    lp.refreshLock.Lock()
    defer lp.refreshLock.Unlock()

    w.Header().Set("content-type", fmt.Sprintf("text/%s; charset=utf-8", lp.contentType))
    w.WriteHeader(200)

    // close any old connection
    if lp.curConnection != nil && lp.curConnection.closed == false{
        close(lp.curConnection.writeQueue)
    }

    // we have written the header, now hijack the connection
    lp.curConnection = &longpollConnection{
        writeQueue: make(chan []byte),
        flush: w.(http.Flusher),
        chunkWriter: w,
    }

    // If the request is to `open`, end the response. The purpose of this is
    // to tell the client that the server is alive. Therefore, the client
    // will fire the open event.
    if params.Get("when") == "open" {
        lp.curConnection.closeLock.Lock()
        defer lp.curConnection.closeLock.Unlock()
        lp.curConnection.closed = true
        close(lp.curConnection.writeQueue)
    } else {
        lp.written = false
        if lp.closeTimer != nil {
            lp.closeTimer.Stop()
        }
        // If aborted is `true` here, it means the user aborted the
        // connection but it couldn't be done because the current response
        // is already closed for other reason. So ends the new exchange.
        if lp.aborted {
            lp.closeConnection()
            return
        }

        // Removes client-received events from the queue. `lastEventIds`
        // param is comma-separated values of id of client-received events.
        eventIds := strings.Split(params.Get("lastEventIds"), ",")
        shorterQue := lp.queue
        if len(eventIds) > 0 {
            for i := range eventIds {
                didBreak := false
                for j := 0; j < len(shorterQue); j++ {
                    if eventIds[i] != shorterQue[j].Id {
                        shorterQue = shorterQue[j:]
                        didBreak = true
                        break
                    }
                }
                if !didBreak {
                    shorterQue = shorterQue[0:0]
                }
            }
            // normalize the changes
            for i := range shorterQue {
                lp.queue[i] = shorterQue[i]
            }
            lp.queue = lp.queue[:len(shorterQue)]
        }

        // If cached events remain in the queue, it indicates the client
        // couldn't receive them.
        for i := range lp.queue {
            b, err := json.Marshal(lp.queue[i])
            if err != nil {
                lp.listener.OnTransportError(err)
            } else {
                lp.SendFromQ(b, true)
            }
        }
    }
}


func (lp *longpollAjax) Send(data []byte) {
    lp.refreshLock.Lock()
    defer lp.refreshLock.Unlock()
    lp.SendFromQ(data, false)
}

func (lp *longpollAjax) SendFromQ(data []byte, fromQueue bool) {
    if !fromQueue {
        var m Message
        // ignoring error, since we did marshall fine in the past
        json.Unmarshal(data, &m)
        lp.queue = append(lp.queue, m)
    }
    // Only when the current response is not closed, it's possible to send.
    // If it is closed, the cached data will be sent in next poll through
    // `refresh` method.
    lp.curConnection.closeLock.Lock()
    defer lp.curConnection.closeLock.Unlock()
    if lp.curConnection.closed {
        return
    }
    lp.written = true
    lp.curConnection.writeQueue <- data
}

func (lp *longpollAjax) Close() {
    lp.aborted = true
    if !lp.doneClose {
        lp.closeConnection()
    }
}

func (lp *longpollAjax) closeConnection() {
    lp.curConnection.closeLock.Lock()
    defer lp.curConnection.closeLock.Unlock()
    if lp.curConnection.closed == false {

        lp.curConnection.closed = true
        close(lp.curConnection.writeQueue)
    }
    if lp.doneClose == true {
        return
    }
    lp.doneClose = true

    if lp.whenParam == "poll" && !lp.written {
        lp.listener.OnTransportClose()
    } else {
        // Otherwise client will issue `poll` request again so it sets a
        // timer to fire close event to prevent this connection from
        // remaining in limbo. 2s is enough.
        lp.closeTimer = time.AfterFunc(2 * time.Second, func () {
            lp.listener.OnTransportClose()
        })
    }
}
