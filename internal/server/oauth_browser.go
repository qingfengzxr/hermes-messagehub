package server

import (
	"html/template"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

var oauthWaitingPage = template.Must(template.New("oauth-waiting").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="refresh" content="2;url={{.ContinueURI}}"><title>MessageHub 通信授权</title></head>
<body><h1>等待管理员批准</h1><p>请将此请求编号提供给管理员核对。不要发送授权链接、密码或令牌。</p>
<p><code>{{.RequestID}}</code></p><p>请保持本页打开，批准后会自动返回原客户端。请求在十分钟后失效。</p>
<p><a href="{{.ContinueURI}}">检查批准状态</a></p></body></html>`))

func oauthPendingHTML(w http.ResponseWriter, r *http.Request, requestID string) bool {
	htmlQuality, jsonQuality := 0.0, 0.0
	for _, item := range strings.Split(r.Header.Get("Accept"), ",") {
		media, params, err := mime.ParseMediaType(strings.TrimSpace(item))
		if err != nil {
			continue
		}
		quality := 1.0
		if raw, ok := params["q"]; ok {
			quality, err = strconv.ParseFloat(raw, 64)
			if err != nil || quality <= 0 || quality > 1 {
				continue
			}
		}
		switch media {
		case "text/html":
			htmlQuality = max(htmlQuality, quality)
		case "application/json":
			jsonQuality = max(jsonQuality, quality)
		}
	}
	if !(htmlQuality > jsonQuality) {
		return false
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	w.WriteHeader(http.StatusAccepted)
	_ = oauthWaitingPage.Execute(w, struct {
		RequestID, ContinueURI string
	}{requestID, "/authorize/continue?request_id=" + url.QueryEscape(requestID)})
	return true
}
