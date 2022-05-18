package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/payfazz/go-errors/v2"
)

func main() {
	err := errors.Catch(func() error {
		lambda.StartHandler(handler{newSvc()})
		return nil
	})

	if err != nil {
		logErr(err)
		os.Exit(1)
	}
}

type handler struct{ *svc }

func (h handler) Invoke(ctx context.Context, in []byte) ([]byte, error) {
	marshalRes := func(r events.APIGatewayV2HTTPResponse) ([]byte, error) {
		out, err := json.Marshal(r)
		if err != nil {
			panic(err)
		}
		return out, nil
	}

	out, err := errors.Catch2(func() ([]byte, error) {
		var req events.APIGatewayV2HTTPRequest
		if err := json.Unmarshal(in, &req); err != nil {
			return marshalRes(text(http.StatusBadRequest, "Not an api gateway request"))
		}

		return marshalRes(h.process(ctx, req))
	})

	if err != nil {
		logErr(err)
		return marshalRes(text(http.StatusInternalServerError, ""))
	}

	return out, nil
}

func logErr(err error) {
	log.Print(errors.FormatWithFilterPkgs(err, "main", "webpage2pdf"))
}
