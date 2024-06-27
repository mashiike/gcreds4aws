# gcreds4aws
Google Cloud Credentials utility for AWS Environment

## Usage 

```go 
package main

import (
	"context"
	"log"

	"github.com/mashiike/gcreds4aws"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

func main() {
	defer gcreds4aws.Close()
	srv, err := sheets.NewService(context.Background(), gcreds4aws.WithCredentials(ctx), option.WithScopes(sheets.SpreadsheetsReadonlyScope))
	if err != nil {
		log.Fatalf("Unable to retrieve Sheets client: %v", err)
	}

	// Google Sheets API Sample
	spreadsheetId := "<your spreadsheet id>"
	readRange := "A1:D5"
	resp, err := srv.Spreadsheets.Values.Get(spreadsheetId, readRange).Do()
	if err != nil {
		log.Fatalf("Unable to retrieve data from sheet: %v", err)
	}
	log.Fatal(resp.Values)
}
```

### Use with AWS SSM Parameter Store

set environment variable `GOOGLE_APPLICATION_CREDENTIALS` to SSM Parameter Store ARN.

```shell
export GOOGLE_APPLICATION_CREDENTIALS="arn:aws:ssm:<region>:<account-id>:parameter/<parameter-name>"
```

Google Cloud Credentials will be fetched from the SSM Parameter Store and cache to in-memory.

### With workload identity pool, (not EC2 instance)

The credentials for using the default workload identity pool are as follows:

```json
{
  "type": "external_account",
  "audiance": "//iam.googleapis.com/projects/<project-number>/locations/global/workloadIdentityPools/<pool-name>/providers/<provider-name>",
  "subject_token_type": "urn:ietf:params:aws:token-type:aws4_request",
  "service_account_impersonation_url": "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/<service-account-email>:generateAccessToken",
  "token_url": "https://sts.googleapis.com/v1/token",
  "credential_source": {
    "environment_id": "aws1",
    "region_url": "http://169.254.169.254/latest/meta-data/placement/availability-zone",
    "url": "http://169.254.169.254/latest/meta-data/iam/security-credentials",
    "regional_cred_verification_url": "https://sts.{region}.amazonaws.com?Action=GetCallerIdentity&Version=2011-06-15"
  }
}
```

However, this is designed to work on EC2 instances and will not function on Lambda or ECS. To work around this, a proxy server can be started locally to simulate the EC2 instance metadata endpoint and provide credential information.

If you want to output the access logs of the internally started HTTP server, set the logger as follows:

```go
gcred4aws.SetLogger(slog.Default())
```
