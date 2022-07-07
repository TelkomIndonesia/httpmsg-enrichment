package main

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/corazawaf/coraza/v2"
	"github.com/gabriel-vasile/mimetype"
	ua "github.com/mileusna/useragent"
	"github.com/telkomindonesia/crs-offline/ecs"
	ecsx "github.com/telkomindonesia/crs-offline/ecs/custom"
)

type enrichment struct {
	ercr *enricher

	tx  *coraza.Transaction
	msg *httpRecordedMessage

	reqMime string
	reqBody *truncatedBuffer
	resMime string
	resBody *truncatedBuffer
}

func detectMime(target *string, reader io.Reader) {
	if target == nil || reader == nil {
		return
	}

	mtype, err := mimetype.DetectReader(reader)
	if err != nil {
		return
	}
	*target = mtype.String()

	io.Copy(io.Discard, reader)
}

func (etx *enrichment) processRequest() (err error) {
	tx := etx.tx
	req, err := etx.msg.Request()
	if err != nil {
		return
	}

	client, port := "", 0
	spl := strings.Split(req.RemoteAddr, ":")
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
	if len(req.TransferEncoding) > 0 {
		tx.AddRequestHeader("Transfer-Encoding", strings.Join(req.TransferEncoding, ","))
	}
	tx.ProcessRequestHeaders()

	// process body
	etx.reqBody = newTruncatedBuffer(int(tx.RequestBodyLimit))
	mimeR, mimeW := io.Pipe()
	defer mimeW.Close()
	go detectMime(&etx.reqMime, mimeR)
	mw := io.MultiWriter(etx.reqBody, mimeW, tx.RequestBodyBuffer)
	if _, err = io.Copy(mw, req.Body); err != nil {
		return fmt.Errorf("error copying request bode: %w", err)
	}
	if _, err := tx.ProcessRequestBody(); err != nil {
		return fmt.Errorf("error processing request: %w", err)
	}

	return
}

func (etx *enrichment) processResponse() (err error) {
	tx := etx.tx
	res, err := etx.msg.Response()
	if err != nil {
		return
	}

	// response header
	for k, v := range res.Header {
		tx.AddResponseHeader(k, strings.Join(v, ","))
	}
	if len(res.TransferEncoding) > 0 {
		tx.AddRequestHeader("Transfer-Encoding", strings.Join(res.TransferEncoding, ","))
	}
	tx.ProcessResponseHeaders(res.StatusCode, res.Proto)

	// response body
	etx.resBody = newTruncatedBuffer(int(tx.RequestBodyLimit))
	mimeR, mimeW := io.Pipe()
	defer mimeW.Close()
	go detectMime(&etx.resMime, mimeR)
	mw := io.MultiWriter(etx.resBody, mimeW, tx.ResponseBodyBuffer)
	if _, err := io.Copy(mw, res.Body); err != nil {
		return fmt.Errorf("error copying response body: %w", err)
	}
	if _, err := tx.ProcessResponseBody(); err != nil {
		return fmt.Errorf("error processing response body: %w", err)
	}

	return
}

func (etx *enrichment) toECS() (doc *ecsx.Document, err error) {
	tx, req, res := etx.tx, etx.msg.req, etx.msg.res
	if req == nil || res == nil {
		return nil, fmt.Errorf("Please invoke ProcessRequest() and ProcessResponse() first.")
	}
	ctx, err := etx.msg.Context()
	if err != nil {
		return nil, fmt.Errorf("error geting context: %w", err)
	}

	toLower := func(m map[string][]string) map[string][]string {
		nm := map[string][]string{}
		for k, v := range m {
			nm[strings.ToLower(k)] = v
		}
		return nm
	}

	doc = &ecsx.Document{
		Document: ecs.Document{
			Base: ecs.Base{
				Message:   "recorded HTTP message",
				Timestamp: ctx.Durations.Proxy.Start,
			},
			ECS: ecs.ECS{
				Version: "8.3.0",
			},
			Event: &ecs.Event{
				Kind:     "event",
				Type:     []string{"access"},
				Category: []string{"web", "authentication", "network"},
				Created:  &ctx.Durations.Total.Start,
				End:      ctx.Durations.Total.End,
				Id:       ctx.ID,
			},
			URL: &ecs.URL{
				Domain:   req.Host,
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
		},

		CRS: &ecsx.CRS{
			Scores: *ecsx.NewScores(etx.tx),
		},

		HTTP: &ecsx.HTTP{
			HTTP: ecs.HTTP{
				Version: fmt.Sprintf("%d.%d", req.ProtoMajor, req.ProtoMinor),
			},
			Request: &ecsx.HTTPRequest{
				HTTPRequest: ecs.HTTPRequest{
					ID:       ctx.ID,
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
				Headers: toLower(req.Header),
			},
			Response: &ecsx.HTTPResponse{
				HTTPResponse: ecs.HTTPResponse{
					StatusCode: res.StatusCode,
					HTTPMessage: ecs.HTTPMessage{
						MimeType: etx.resMime,
						Body: &ecs.HTTPMessageBody{
							Bytes:   int64(etx.resBody.Len()),
							Content: etx.resBody.String(),
						},
					},
				},
				Headers: toLower(res.Header),
			},
		},
	}

	if v := req.Header.Get("user-agent"); v != "" {
		uap := ua.Parse(v)
		doc.UserAgent = &ecs.UserAgent{
			Original: v,
			Name:     uap.Name,
			Version:  uap.Version,
			Device: &ecs.UserAgentDevice{
				Name: uap.Device,
			},
			OS: &ecs.OS{
				Name:    uap.OS,
				Version: uap.OSVersion,
			},
		}
	}

	if ctx != nil && ctx.Credential != nil {
		doc.User = &ecs.User{
			Name: ctx.Credential.Username,
		}
	}

	if ctx != nil && ctx.Connection != nil && etx.ercr.geoDB != nil {
		record, err := etx.ercr.geoDB.City(ctx.Connection.Client.IP)
		if err == nil {
			doc.Client = &ecs.Endpoint{
				IP: ctx.Connection.Client.IP,
				Geo: ecs.Geo{
					CityName: record.City.Names["en"],

					CountryName:    record.Country.Names["en"],
					CountryISOCode: record.Country.IsoCode,

					ContinentName: record.Continent.Names["en"],
					ContinentCode: record.Continent.Code,
					Location: &ecs.GeoPoint{
						Lon: record.Location.Longitude,
						Lat: record.Location.Latitude,
					},
					PostalCode: record.Postal.Code,
					Timezone:   record.Location.TimeZone,
				},
			}
		}
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

func (etx enrichment) Close() {
	etx.msg.req.Body.Close()
	etx.msg.res.Body.Close()
	etx.tx.ProcessLogging()
	etx.tx.Clean()
}