package main

import (
	"context"
	"log"
	"os"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/payfazz/go-errors/v2"
)

func main() {
	if err := errors.Catch(func() error {
		svc := newSvc()
		lambda.Start(func(ctx context.Context, req events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
			return svc.process(ctx, req), nil
		})
		return nil
	}); err != nil {
		logErr(err)
		os.Exit(1)
	}
}

func logErr(err error) {
	log.Print(errors.FormatWithFilterPkgs(err, "main"))
}
