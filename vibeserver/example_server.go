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

package main


import (
    "github.com/fromkeith/vibe"
    "net/http"
    "log"
    "strings"
    "strconv"
    "encoding/json"
    "time"
)


func main() {

    var sl myServerListener
    v := vibe.NewServer(nil, 0)
    v.Listener = sl

    http.HandleFunc("/setup", func (w http.ResponseWriter, req *http.Request) {
        log.Println("Setup: ", req.URL.RawQuery)
        trans := req.URL.Query().Get("transports")
        transSplit := strings.Split(trans, ",")

        transports := make([]vibe.TransportType, len(transSplit))
        k := 0
        for i := range transports {
            if transSplit[i] == "" {
                continue
            }
            k ++
            transports[i] = vibe.TransportType(transSplit[i])
        }
        v.SetTransports(transports[:k])

        heart := req.URL.Query().Get("heartbeat")
        if heart != "" {
            heartbeat, _ := strconv.ParseInt(heart, 10, 32)
            v.SetHeartbeat(time.Millisecond * time.Duration(heartbeat))
        }

        defer req.Body.Close()
        defer w.WriteHeader(200)
    })
    http.Handle("/vibe", v)
    http.HandleFunc("/alive", func (w http.ResponseWriter, req *http.Request) {
        id := req.URL.Query().Get("id")
        defer req.Body.Close()
        w.WriteHeader(200)
        res := v.IsSocketAlive(id)
        asStr := strconv.FormatBool(res)
        w.Write([]byte(asStr))
        log.Println("Alive:", id, asStr)
    })

    http.ListenAndServe(":8000", nil)

}

type myServerListener struct {

}

func (serv myServerListener) Socket(s *vibe.VibeSocket) {
    var ms mySocketListener
    ms.socket = s
    s.Listener = ms
}

type mySocketListener struct {
    socket      *vibe.VibeSocket
}

func (serv mySocketListener) Error(err error) {
    log.Println("Error: ", err)
}
func (serv mySocketListener) Close() {
    log.Println("Close!")
}
func (serv mySocketListener) Messsage(t string, d interface{}) {
    log.Println("Message!", t, d)
    if t == "echo" {
        serv.socket.Send("echo", d, nil)
    } else if t == "/reply/outbound" {
        dataB, _ := json.Marshal(d)
        var data vibe.Message
        json.Unmarshal(dataB, &data)
        if data.Type == "resolved" {
            serv.socket.Send("test", data.Data, func (resolve bool, value interface{}) {
                log.Println("callback fired!")
                if resolve != true {
                    panic("did not resolve")
                }
                serv.socket.Send("done", value, nil)
            })
        } else {
            serv.socket.Send("test", data.Data, func (resolve bool, value interface{}) {
                log.Println("callback2 fired!")
                if resolve == true {
                    panic("did not exception")
                }
                serv.socket.Send("done", value, nil)
            })
        }
    }
}
func (serv mySocketListener) ReplyMessage(t string, d interface{}, res func (resolve bool, value interface{})) {
    log.Println("MessageReply!", t, d)
    if t == "/reply/inbound" {
        dataB, _ := json.Marshal(d)
        var data vibe.Message
        json.Unmarshal(dataB, &data)
        if data.Type == "resolved" {
            res(true, data.Data)
        } else {
            res(false, data.Data)
        }
    }
}