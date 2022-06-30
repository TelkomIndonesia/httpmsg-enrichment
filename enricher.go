package main

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/corazawaf/coraza/v2"
	"github.com/corazawaf/coraza/v2/seclang"
	"github.com/gabriel-vasile/mimetype"
	"github.com/telkomindonesia/crs-offline/ecs"
)

type enricherTransaction struct {
	tx  *coraza.Transaction
	msg *httpRecordedMessage

	reqMime string
	reqBody *truncatedBuffer
	resMime string
	resBody *truncatedBuffer
}

func (etx *enricherTransaction) processRequest() (err error) {
	tx := etx.tx
	req, err := etx.msg.Request()
	if err != nil {
		return
	}

	raddr := req.RemoteAddr
	if raddr == "" {
		raddr = req.Header.Get("x-forwarded-for")
	}
	client, port := "", 0
	spl := strings.Split(raddr, ":")
	if len(spl) > 0 {
		client = strings.Join(spl[0:len(spl)-1], "")
	}
	if len(spl) > 1 {
		port, _ = strconv.Atoi(spl[len(spl)-1])
	}
	tx.ProcessConnection(client, port, "", 0)

	// process uri
	tx.ProcessURI(req.URL.String(), req.Method, req.Proto)

	// process header
	for k, vr := range req.Header {
		for _, v := range vr {
			tx.AddRequestHeader(k, v)
		}
	}
	if req.Host != "" {
		tx.AddRequestHeader("Host", req.Host)
	}
	tx.ProcessRequestHeaders()

	// process body
	etx.reqBody = newTruncatedBuffer(int(tx.RequestBodyLimit))
	mimer, mimew := io.Pipe()
	defer mimew.Close()
	go func() {
		mtype, err := mimetype.DetectReader(mimer)
		if err != nil {
			return
		}
		etx.reqMime = mtype.String()
	}()
	mw := io.MultiWriter(etx.reqBody, mimew, tx.RequestBodyBuffer)
	if _, err = io.Copy(mw, req.Body); err != nil {
		return fmt.Errorf("error copying request bode: %w", err)
	}
	if _, err := tx.ProcessRequestBody(); err != nil {
		return fmt.Errorf("error processing request: %w", err)
	}
	mimew.Close()

	req.Body = etx.reqBody
	return
}

func (etx *enricherTransaction) processResponse() (err error) {
	tx := etx.tx
	res, err := etx.msg.Response()
	if err != nil {
		return
	}

	// response header
	for k, v := range res.Header {
		tx.AddResponseHeader(k, strings.Join(v, ","))
	}
	tx.ProcessResponseHeaders(res.StatusCode, res.Proto)

	// response body
	etx.resBody = newTruncatedBuffer(int(tx.RequestBodyLimit))
	mimer, mimew := io.Pipe()
	defer mimew.Close()
	go func() {
		mtype, err := mimetype.DetectReader(mimer)
		if err != nil {
			return
		}
		etx.resMime = mtype.String()
	}()
	mw := io.MultiWriter(etx.resBody, mimew, tx.ResponseBodyBuffer)
	if _, err := io.Copy(mw, res.Body); err != nil || !tx.IsProcessableResponseBody() {
		return fmt.Errorf("error copying response body: %w", err)
	}

	if _, err := tx.ProcessResponseBody(); err != nil {
		return fmt.Errorf("error processing response body: %w", err)
	}
	res.Body = etx.resBody

	return
}
func (etx *enricherTransaction) toScore() *scores {
	return newScores(etx.tx)
}

func (etx *enricherTransaction) toECS() (doc *ecs.Document, err error) {
	tx, req, res := etx.tx, etx.msg.req, etx.msg.res
	if req == nil || res == nil {
		return nil, fmt.Errorf("Please invoke ProcessRequest() and ProcessResponse() first.")
	}

	doc = &ecs.Document{
		Base: ecs.Base{
			Message: "recorded HTTP message",
		},
		HTTP: &ecs.HTTP{
			Version: fmt.Sprintf("%d.%d", req.ProtoMajor, req.ProtoMinor),
			Request: &ecs.HTTPRequest{
				ID:       req.Header.Get("x-request-id"),
				Method:   req.Method,
				Referrer: req.Referer(),
				HTTPMessage: ecs.HTTPMessage{
					MimeType: etx.reqMime,
					Body: &ecs.HTTPMessageBody{
						Bytes:   int64(etx.reqBody.Len()),
						Content: etx.reqBody.String(),
					},
				},
			},
			Response: &ecs.HTTPResponse{
				StatusCode: res.StatusCode,
				HTTPMessage: ecs.HTTPMessage{
					MimeType: etx.reqMime,
					Body: &ecs.HTTPMessageBody{
						Bytes:   int64(etx.resBody.Len()),
						Content: etx.resBody.String(),
					},
				},
			},
		},
		URL: &ecs.URL{
			Domain:   req.URL.Host,
			Full:     req.URL.String(),
			Original: req.URL.String(),
			Query:    req.URL.Query().Encode(),
			Fragment: req.URL.Fragment,
			Path:     req.URL.Path,
			Scheme:   req.URL.Scheme,
		},
		Threat: &ecs.Threat{
			Enrichments: []ecs.ThreatEnrichments{},
		},
	}

	for _, rule := range tx.MatchedRules {
		idc := ecs.ThreatIndicator{
			Description: rule.ErrorLog(0),
			IP:          net.ParseIP(rule.ClientIPAddress),
			Provider:    rule.Rule.Version,
			Type:        "network-traffic",
		}
		match := &ecs.ThreatEnrichmentMatch{
			Type:   "indicator_match_rule",
			Atomic: Truncate(rule.MatchedData.Value, 200),
			Field:  rule.MatchedData.VariableName,
		}
		atk := false
		for _, tag := range rule.Rule.Tags {
			atk = atk || strings.HasPrefix(tag, "attack-")

			pl := strings.TrimPrefix(tag, "paranoia-level/")
			if pl == "" {
				continue
			}

			i, _ := strconv.ParseInt(pl, 10, 8)
			switch i {
			case 1:
				idc.Confidence = "High"
			case 2:
				idc.Confidence = "Medium"
			case 3, 4:
				idc.Confidence = "Low"
			default:
				idc.Confidence = "Not Specified"
			}
		}
		if !atk {
			continue
		}

		doc.Threat.Enrichments = append(doc.Threat.Enrichments, ecs.ThreatEnrichments{
			Indicator: idc,
			Match:     match,
		})
	}
	return
}

func (etx enricherTransaction) Close() {
	etx.msg.req.Body.Close()
	etx.msg.res.Body.Close()
	etx.tx.ProcessLogging()
	etx.tx.Clean()
}

type enricher struct {
	waf *coraza.Waf
}

func newEnricher() (cw enricher, err error) {
	waf := coraza.NewWaf()
	parser, _ := seclang.NewParser(waf)
	for _, f := range []string{"crs/coraza.conf", "crs/crs-setup.conf", "crs/rules/*.conf"} {
		if err := parser.FromFile(f); err != nil {
			return cw, fmt.Errorf("error reading rules file from %s: %w", f, err)
		}
	}
	return enricher{waf}, nil
}

func (er enricher) newTransaction(record io.Reader) *enricherTransaction {
	return &enricherTransaction{
		tx:  er.waf.NewTransaction(),
		msg: newHTTPRecordedMessage(record),
	}
}

func (er enricher) EnrichRecord(record io.Reader) (tx *enricherTransaction, err error) {
	tx = er.newTransaction(record)

	err = tx.processRequest()
	if err != nil {
		return nil, fmt.Errorf("error parsing request: %w", err)
	}
	err = tx.processResponse()
	if err != nil {
		return nil, fmt.Errorf("error parsing response: %w", err)
	}

	return
}
