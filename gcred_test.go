package gcreds4aws

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type MockSSMClient struct {
	mock.Mock
}

func (m *MockSSMClient) GetParameter(ctx context.Context, input *ssm.GetParameterInput, optFns ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	args := m.Called(ctx, input)
	return args.Get(0).(*ssm.GetParameterOutput), args.Error(1)
}

func TestNewCredentialsOptionWithSSM(t *testing.T) {
	mockClient := new(MockSSMClient)
	mockClient.On("GetParameter", mock.Anything, mock.MatchedBy(
		func(input *ssm.GetParameterInput) bool {
			return *input.Name == "test-parameter"
		},
	)).Return(&ssm.GetParameterOutput{
		Parameter: &types.Parameter{
			Value: aws.String(`{"type":"external_account","audience":"//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/pool/providers/provider","subject_token_type":"urn:ietf:params:aws:token-type:aws4_request","service_account_impersonation_url":"https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/service-account-email:generateAccessToken","token_url":"https://sts.googleapis.com/v1/token"}`),
		},
	}, nil)

	SetSSMClient(mockClient)

	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "arn:aws:ssm:us-east-1:123456789012:parameter/test-parameter")

	ctx := context.Background()
	opt, err := NewCredentials(ctx)
	require.NoError(t, err)
	require.NotNil(t, opt)
}
