package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/payfazz/go-errors/v2"
)

const loopCheckKey = "f87e51e5252d314330469b6ba2206341"
const tabTimeout = 5 * time.Second
const region = "ap-southeast-1"
const bucket = "webpage2pdf"
const presignExpiration = 12 * time.Hour

const authEnvKey = "WEBPAGE2PDF_KEY"

type svc struct {
	chromeCtx context.Context

	uploader *manager.Uploader
	psClient *s3.PresignClient

	authKey string
}

func newSvc() *svc {
	var execOpt []chromedp.ExecAllocatorOption
	execOpt = append(execOpt, chromedp.DefaultExecAllocatorOptions[:]...)
	execOpt = append(execOpt,
		chromedp.NoSandbox,
		chromedp.Flag("no-zygote", true),
		chromedp.Flag("in-process-gpu", true),
		chromedp.Flag("single-process", true),
	)

	chromeCtx, _ := chromedp.NewExecAllocator(context.Background(), execOpt...)
	chromeCtx, _ = chromedp.NewContext(chromeCtx)

	if err := chromedp.Run(chromeCtx); err != nil {
		panic(errors.Errorf("cannot launch headless chrome: %w", err))
	}

	awsConfig, err := config.LoadDefaultConfig(
		context.Background(),
		config.WithRegion(region),
	)
	if err != nil {
		panic(errors.Errorf("cannot get aws config: %w", err))
	}

	client := s3.NewFromConfig(awsConfig)

	return &svc{
		chromeCtx: chromeCtx,

		uploader: manager.NewUploader(client),
		psClient: s3.NewPresignClient(client),

		authKey: os.Getenv(authEnvKey),
	}
}

func (s *svc) process(ctx context.Context, req events.APIGatewayV2HTTPRequest) events.APIGatewayV2HTTPResponse {
	if req.RawPath != "/" && req.RawPath != "" {
		return text(http.StatusNotFound, "")
	}
	if req.RequestContext.HTTP.Method != http.MethodGet {
		return text(http.StatusMethodNotAllowed, "")
	}

	authHeader := req.Headers["authorization"]
	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token != s.authKey {
		return text(http.StatusUnauthorized, "")
	}

	query, err := url.ParseQuery(req.RawQueryString)
	if err != nil {
		return text(http.StatusBadRequest, "cannot parse query string")
	}

	if query.Get(loopCheckKey) != "" {
		return text(http.StatusBadRequest, "loop detected")
	}

	targets := query["target"]
	if len(targets) != 1 {
		return text(http.StatusBadRequest, "\"target\" param must be exactly one")
	}

	target, err := url.Parse(targets[0])
	if err != nil {
		return text(http.StatusBadRequest, "invalid \"target\"")
	}

	if target.Scheme != "http" && target.Scheme != "https" {
		return text(http.StatusBadRequest, "target must be \"http\" or \"https\"")
	}

	targetParams := target.Query()
	targetParams.Set(loopCheckKey, "1")
	target.RawQuery = targetParams.Encode()

	timeoutCtx, cancelTimeout := context.WithTimeout(context.Background(), tabTimeout)
	defer cancelTimeout()

	tabCtx, cancelTabCtx := chromedp.NewContext(s.chromeCtx)
	defer func() { <-tabCtx.Done() }()
	cancelTab := func() {
		chromedp.Cancel(tabCtx)
		cancelTabCtx()
	}
	defer cancelTab()

	go func() {
		select {
		case <-ctx.Done():
		case <-timeoutCtx.Done():
		case <-tabCtx.Done():
		}
		cancelTab()
		cancelTimeout()
	}()

	reqs := make(map[network.RequestID]string)
	var reqsErr []string
	chromedp.ListenTarget(tabCtx, func(ev interface{}) {
		switch ev := ev.(type) {
		case *network.EventRequestWillBeSent:
			reqs[ev.RequestID] = fmt.Sprintf("%s %s", ev.Request.Method, ev.Request.URL)
		case *network.EventResponseReceived:
			if ev.Response.Status >= 400 {
				reqsErr = append(reqsErr, fmt.Sprintf("%s: status code %d", reqs[ev.RequestID], ev.Response.Status))
				cancelTab()
			}
		case *network.EventLoadingFailed:
			reqsErr = append(reqsErr, fmt.Sprintf("%s: loading failed", reqs[ev.RequestID]))
			cancelTab()
		}
	})

	var pdfBytes []byte
	err = chromedp.Run(tabCtx,
		// navigate
		chromedp.Navigate(target.String()),

		// wait document ready
		chromedp.EvaluateAsDevTools(``+
			`window.webpage2pdfwait||`+
			`new Promise(r=>{if(document.readyState=='complete')r();else window.addEventListener('load',r)})`,
			nil,
			func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) },
		),

		// get pdf
		chromedp.ActionFunc(func(ctx context.Context) error {
			params := page.PrintToPDF()
			params.PreferCSSPageSize = true

			var err error
			pdfBytes, _, err = params.Do(ctx)
			if err != nil {
				return errors.Trace(err)
			}

			return nil
		}),
	)

	if len(reqsErr) > 0 {
		var sb strings.Builder
		sb.WriteString("target must be complete page, but we got ")
		sb.WriteString(strconv.Itoa(len(reqsErr)))
		sb.WriteString(" error:\n")
		for _, e := range reqsErr {
			sb.WriteString(e)
			sb.WriteByte('\n')
		}
		return text(http.StatusBadRequest, sb.String())
	}

	if err != nil {
		if errors.Is(timeoutCtx.Err(), context.DeadlineExceeded) {
			return targetTimeout()
		}
		return internalErr(errors.Trace(err))
	}

	go cancelTab()

	key := randomName()
	_, err = s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:      ref(bucket),
		Key:         ref(key),
		ContentType: ref("application/pdf"),
		Body:        bytes.NewReader(pdfBytes),
	})
	if err != nil {
		return internalErr(errors.Trace(err))
	}

	presignedURL, err := s.psClient.PresignGetObject(ctx,
		&s3.GetObjectInput{
			Bucket: ref(bucket),
			Key:    ref(key),
		},
		func(o *s3.PresignOptions) {
			o.Expires = presignExpiration
		},
	)
	if err != nil {
		return internalErr(errors.Trace(err))
	}

	return found(presignedURL.URL)
}

func text(code int, msg string) events.APIGatewayV2HTTPResponse {
	if msg == "" {
		msg = http.StatusText(code)
	}
	return events.APIGatewayV2HTTPResponse{
		StatusCode: code,
		Headers:    map[string]string{"content-type": "text/plain"},
		Body:       msg,
	}
}

func internalErr(err error) events.APIGatewayV2HTTPResponse {
	logErr(err)
	return text(http.StatusInternalServerError, "")
}

func targetTimeout() events.APIGatewayV2HTTPResponse {
	return text(http.StatusBadRequest, "cannot process target webpage in "+tabTimeout.Truncate(time.Second).String())
}

func found(target string) events.APIGatewayV2HTTPResponse {
	return events.APIGatewayV2HTTPResponse{
		StatusCode: http.StatusFound,
		Headers:    map[string]string{"location": target},
	}
}

func randomName() string {
	var value [16]byte
	_, err := io.ReadFull(rand.Reader, value[:])
	if err != nil {
		panic(err)
	}
	return hex.EncodeToString(value[:])
}

func ref[T any](t T) *T {
	return &t
}
