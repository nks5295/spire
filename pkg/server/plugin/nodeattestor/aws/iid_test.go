package aws

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/spiffe/spire/pkg/common/pemutil"

	"github.com/spiffe/spire/pkg/common/plugin/aws"
	caws "github.com/spiffe/spire/pkg/common/plugin/aws"
	mock_aws "github.com/spiffe/spire/test/mock/server/aws"

	awssdk "github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/golang/mock/gomock"
	"github.com/spiffe/spire/proto/common"
	"github.com/spiffe/spire/proto/common/plugin"
	"github.com/spiffe/spire/proto/server/nodeattestor"
	"github.com/stretchr/testify/suite"
)

const (
	testRSAKey = `-----BEGIN RSA PRIVATE KEY-----
MIIBzAIBAAJhAMnVzWSZn20CtcFaWh1Uuoh7NObRt9z84h8zzuIVSNkeJV6Dei0v
8FGp3ZilrU3MDM6WsuFTUVo21qBTOTnYKuEI0bk7pTgZk9CN6aF0iZbzyrvsU6hy
b09dN0PFBc5A2QIDAQABAmEAqSpioQvFPKfF0M46s1S9lwC1ATULRtRJbd+NaZ5v
VVLX/VRzRYZlhPy7d2J9U7ROFjSM+Fng8S1knrHAK0ka/ZfYOl1ZLoMexpBovebM
mGcsCHrHz4eBN8B1Y+8JRhkBAjEA7fTLjbz3M7za1nGODqWsoBv33yJHGh9GIaf9
umpx3qpFZCVsqHgCvmalAu+IXAz5AjEA2SPTRcddrGVsDnSOYot3eCArVOIxgI+r
H9A4cjS4cp4W4nBZhb+08/IYtDfYdirhAjAtl8LMtJE045GWlwld+xZ5UwKKSVoQ
Qj/AwRxXdH++5ycGijkoil4UNzyUtGqPIJkCMQC5g9ola8ekWqKPVxWvK+jOQO3E
f9w7MoPJkmQnbtOHWXnDzKkvlDJNmTFyB6RwkQECMQDp+GR2I305amG9isTzm7UU
8pJxbXLymDwR4A7x5vwH6x2gLBgpat21QAR14W4dYEg=
-----END RSA PRIVATE KEY-----`
)

var (
	testInstance = "test-instance"
	testAccount  = "test-account"
	testRegion   = "test-region"
)

func TestIIDAttestorPlugin(t *testing.T) {
	suite.Run(t, new(IIDAttestorSuite))
}

type IIDAttestorSuite struct {
	suite.Suite

	// original plugin, for modifications on mock
	plugin *IIDAttestorPlugin
	// built-in for full callstack
	p      *nodeattestor.BuiltIn
	rsaKey *rsa.PrivateKey
	env    map[string]string
}

func (s *IIDAttestorSuite) SetupTest() {
	rsaKey, err := pemutil.ParseRSAPrivateKey([]byte(testRSAKey))
	s.Require().NoError(err)
	s.rsaKey = rsaKey

	s.env = make(map[string]string)

	p := NewIIDPlugin()
	p.hooks.getEnv = func(key string) string {
		return s.env[key]
	}
	s.plugin = p
	s.p = nodeattestor.NewBuiltIn(p)

	_, err = s.p.Configure(context.Background(), &plugin.ConfigureRequest{
		Configuration: ``,
		GlobalConfig:  &plugin.ConfigureRequest_GlobalConfig{TrustDomain: "example.org"},
	})
	s.Require().NoError(err)
}

func (s *IIDAttestorSuite) TestErrorWhenNotConfigured() {
	p := nodeattestor.NewBuiltIn(NewIIDPlugin())
	stream, err := p.Attest(context.Background())
	defer stream.CloseSend()
	resp, err := stream.Recv()
	s.requireErrorContains(err, "not configured")
	s.Require().Nil(resp)
}

func (s *IIDAttestorSuite) TestErrorOnEmptyRequest() {
	_, err := s.attest(&nodeattestor.AttestRequest{})
	s.requireErrorContains(err, "request missing attestation data")
}

func (s *IIDAttestorSuite) TestErrorOnInvalidType() {
	_, err := s.attest(&nodeattestor.AttestRequest{
		AttestationData: &nodeattestor.AttestationData{
			Type: "foo",
		},
	})
	s.requireErrorContains(err, `unexpected attestation data type "foo"`)
}

func (s *IIDAttestorSuite) TestErrorOnMissingData() {
	data := &common.AttestationData{
		Type: aws.PluginName,
	}

	_, err := s.attest(&nodeattestor.AttestRequest{AttestationData: data})
	s.requireErrorContains(err, "unexpected end of JSON input")
}

func (s *IIDAttestorSuite) TestErrorOnBadData() {
	data := &common.AttestationData{
		Type: aws.PluginName,
		Data: make([]byte, 0),
	}

	_, err := s.attest(&nodeattestor.AttestRequest{AttestationData: data})
	s.requireErrorContains(err, "unexpected end of JSON input")
}

func (s *IIDAttestorSuite) TestErrorOnAlreadyAttested() {
	data := &common.AttestationData{
		Type: aws.PluginName,
		Data: s.iidAttestationDataToBytes(*s.buildDefaultIIDAttestationData()),
	}

	_, err := s.attest(&nodeattestor.AttestRequest{
		AttestationData: data,
		AttestedBefore:  true,
	})
	s.requireErrorContains(err, "the IID has been used and is no longer valid")
}

func (s *IIDAttestorSuite) TestErrorOnBadSignature() {
	iid := s.buildDefaultIIDAttestationData()
	iid.Signature = "bad sig"
	data := &common.AttestationData{
		Type: aws.PluginName,
		Data: s.iidAttestationDataToBytes(*iid),
	}

	_, err := s.attest(&nodeattestor.AttestRequest{
		AttestationData: data,
	})
	s.requireErrorContains(err, "illegal base64 data at input byte")
}

func (s *IIDAttestorSuite) TestErrorOnNoSignature() {
	iid := s.buildDefaultIIDAttestationData()
	iid.Signature = ""
	data := &common.AttestationData{
		Type: aws.PluginName,
		Data: s.iidAttestationDataToBytes(*iid),
	}

	_, err := s.attest(&nodeattestor.AttestRequest{
		AttestationData: data,
	})
	s.requireErrorContains(err, "verifying the cryptographic signature")
}

func (s *IIDAttestorSuite) TestClientAndIDReturns() {
	zeroDeviceIndex := int64(0)
	nonzeroDeviceIndex := int64(1)
	instanceStoreType := ec2.DeviceTypeInstanceStore
	ebsStoreType := ec2.DeviceTypeEbs

	tests := []struct {
		desc                string
		mockExpect          func(mock *mock_aws.MockEC2Client)
		expectID            string
		expectErr           string
		replacementTemplate string
		skipEC2             bool
		skipBlockDev        bool
	}{
		{
			desc: "error on call",
			mockExpect: func(mock *mock_aws.MockEC2Client) {
				mock.EXPECT().DescribeInstancesWithContext(gomock.Any(), &ec2.DescribeInstancesInput{
					InstanceIds: []*string{&testInstance},
				}).Return(nil, errors.New("client error"))
			},
			expectErr: "client error",
		},
		{
			desc: "non-zero device index",
			mockExpect: func(mock *mock_aws.MockEC2Client) {
				output := getDefaultDescribeInstancesOutput()
				output.Reservations[0].Instances[0].RootDeviceType = &instanceStoreType
				output.Reservations[0].Instances[0].NetworkInterfaces[0].Attachment.DeviceIndex = &nonzeroDeviceIndex
				mock.EXPECT().DescribeInstancesWithContext(gomock.Any(), &ec2.DescribeInstancesInput{
					InstanceIds: []*string{&testInstance},
				}).Return(&output, nil)
			},
			expectErr: "verifying the EC2 instance's NetworkInterface[0].DeviceIndex is 0",
		},
		{
			desc: "success, client, no block device, default template",
			mockExpect: func(mock *mock_aws.MockEC2Client) {
				output := getDefaultDescribeInstancesOutput()
				output.Reservations[0].Instances[0].RootDeviceType = &ebsStoreType
				output.Reservations[0].Instances[0].NetworkInterfaces[0].Attachment.DeviceIndex = &zeroDeviceIndex
				mock.EXPECT().DescribeInstancesWithContext(gomock.Any(), &ec2.DescribeInstancesInput{
					InstanceIds: []*string{&testInstance},
				}).Return(&output, nil)
			},
			skipBlockDev: true,
			expectID:     "spiffe://example.org/spire/agent/aws_iid/test-account/test-region/test-instance",
		},
		{
			desc:     "success, no client call, default template",
			skipEC2:  true,
			expectID: "spiffe://example.org/spire/agent/aws_iid/test-account/test-region/test-instance",
		},
		{
			desc: "success, client + block device, default template",
			mockExpect: func(mock *mock_aws.MockEC2Client) {
				output := getDefaultDescribeInstancesOutput()
				output.Reservations[0].Instances[0].RootDeviceType = &instanceStoreType
				output.Reservations[0].Instances[0].NetworkInterfaces[0].Attachment.DeviceIndex = &zeroDeviceIndex
				mock.EXPECT().DescribeInstancesWithContext(gomock.Any(), &ec2.DescribeInstancesInput{
					InstanceIds: []*string{&testInstance},
				}).Return(&output, nil)
			},
			expectID: "spiffe://example.org/spire/agent/aws_iid/test-account/test-region/test-instance",
		},
		{
			desc: "success, client + block device, different template",
			mockExpect: func(mock *mock_aws.MockEC2Client) {
				output := getDefaultDescribeInstancesOutput()
				output.Reservations[0].Instances[0].RootDeviceType = &instanceStoreType
				output.Reservations[0].Instances[0].NetworkInterfaces[0].Attachment.DeviceIndex = &zeroDeviceIndex
				mock.EXPECT().DescribeInstancesWithContext(gomock.Any(), &ec2.DescribeInstancesInput{
					InstanceIds: []*string{&testInstance},
				}).Return(&output, nil)
			},
			replacementTemplate: "{{ .PluginName}}/{{ .Region }}/{{ .AccountID }}/{{ .InstanceID }}",
			expectID:            "spiffe://example.org/spire/agent/aws_iid/test-region/test-account/test-instance",
		},
	}

	for _, tt := range tests {
		s.T().Run(tt.desc, func(t *testing.T) {
			mockCtl := gomock.NewController(s.T())
			defer mockCtl.Finish()

			ec2Client := mock_aws.NewMockEC2Client(mockCtl)

			originalGetEC2Client := s.plugin.hooks.getClient
			defer func() {
				s.plugin.hooks.getClient = originalGetEC2Client
			}()
			mockGetEC2Client := func(p client.ConfigProvider, cfgs ...*awssdk.Config) EC2Client {
				return ec2Client
			}
			s.plugin.hooks.getClient = mockGetEC2Client
			if tt.mockExpect != nil {
				tt.mockExpect(ec2Client)
			}

			var configStr string
			if tt.replacementTemplate != "" {
				configStr = fmt.Sprintf(`agent_path_template = "%s"`, tt.replacementTemplate)
			}
			if tt.skipEC2 {
				configStr = configStr + "\nskip_ec2_attest_calling = true"
			}
			if tt.skipBlockDev {
				configStr = configStr + "\nskip_block_device = true"
			}

			_, err := s.p.Configure(context.Background(), &plugin.ConfigureRequest{
				Configuration: configStr,
				GlobalConfig:  &plugin.ConfigureRequest_GlobalConfig{TrustDomain: "example.org"},
			})
			s.Require().NoError(err)

			data := &common.AttestationData{
				Type: aws.PluginName,
				Data: s.iidAttestationDataToBytes(*s.buildDefaultIIDAttestationData()),
			}

			// using our own keypair (since we don't have AWS private key)
			originalAWSPublicKey := s.plugin.config.awsCaCertPublicKey
			defer func() {
				s.plugin.config.awsCaCertPublicKey = originalAWSPublicKey
			}()
			s.plugin.config.awsCaCertPublicKey = &s.rsaKey.PublicKey

			resp, err := s.attest(&nodeattestor.AttestRequest{
				AttestationData: data,
			})

			if tt.expectErr != "" {
				s.Nil(resp)
				s.requireErrorContains(err, tt.expectErr)
				return
			}

			s.True(resp.Valid)
			s.Equal(tt.expectID, resp.BaseSPIFFEID)
		})
	}
}

func (s *IIDAttestorSuite) TestErrorOnBadSVIDTemplate() {
	_, err := s.p.Configure(context.Background(), &plugin.ConfigureRequest{
		Configuration: `
agent_path_template = "{{ .InstanceID "
`,
		GlobalConfig: &plugin.ConfigureRequest_GlobalConfig{TrustDomain: "example.org"},
	})
	s.requireErrorContains(err, "failed to parse agent svid template")
}

func (s *IIDAttestorSuite) TestConfigure() {
	require := s.Require()

	// malformed
	resp, err := s.p.Configure(context.Background(), &plugin.ConfigureRequest{
		Configuration: `trust_domain`,
		GlobalConfig:  &plugin.ConfigureRequest_GlobalConfig{TrustDomain: "example.org"},
	})
	s.requireErrorContains(err, "expected start of object")
	require.Nil(resp)

	// missing global configuration
	resp, err = s.p.Configure(context.Background(), &plugin.ConfigureRequest{
		Configuration: ``})
	s.requireErrorContains(err, "global configuration is required")
	require.Nil(resp)

	// missing trust domain
	resp, err = s.p.Configure(context.Background(), &plugin.ConfigureRequest{
		Configuration: ``,
		GlobalConfig:  &plugin.ConfigureRequest_GlobalConfig{}})
	s.requireErrorContains(err, "trust_domain is required")
	require.Nil(resp)

	// fails with access id but no secret
	resp, err = s.plugin.Configure(context.Background(), &plugin.ConfigureRequest{
		Configuration: `
		access_key_id = "ACCESSKEYID"
		`,
		GlobalConfig: &plugin.ConfigureRequest_GlobalConfig{TrustDomain: "example.org"}})
	s.Require().EqualError(err, "configuration missing secret access key, but has access key id")
	s.Require().Nil(resp)

	// fails with secret but no access id
	resp, err = s.plugin.Configure(context.Background(), &plugin.ConfigureRequest{
		Configuration: `
		secret_access_key = "SECRETACCESSKEY"
		`,
		GlobalConfig: &plugin.ConfigureRequest_GlobalConfig{TrustDomain: "example.org"}})
	s.Require().EqualError(err, "configuration missing access key id, but has secret access key")
	s.Require().Nil(resp)

	// success with envvars
	s.env[caws.AccessKeyIDVarName] = "ACCESSKEYID"
	s.env[caws.SecretAccessKeyVarName] = "SECRETACCESSKEY"
	resp, err = s.plugin.Configure(context.Background(), &plugin.ConfigureRequest{
		GlobalConfig: &plugin.ConfigureRequest_GlobalConfig{TrustDomain: "example.org"},
	})
	s.Require().NoError(err)
	s.Require().Equal(resp, &plugin.ConfigureResponse{})
	delete(s.env, caws.AccessKeyIDVarName)
	delete(s.env, caws.SecretAccessKeyVarName)

	// success, no AWS keys
	resp, err = s.p.Configure(context.Background(), &plugin.ConfigureRequest{
		Configuration: ``,
		GlobalConfig:  &plugin.ConfigureRequest_GlobalConfig{TrustDomain: "example.org"}})
	require.NoError(err)
	require.NotNil(resp)
	require.Equal(resp, &plugin.ConfigureResponse{})
}

func (s *IIDAttestorSuite) TestGetPluginInfo() {
	require := s.Require()
	resp, err := s.p.GetPluginInfo(context.Background(), &plugin.GetPluginInfoRequest{})
	require.NoError(err)
	require.NotNil(resp)
	require.Equal(resp, &plugin.GetPluginInfoResponse{})
}

// Test helpers

type recvFailStream struct {
	nodeattestor.Attest_PluginStream
}

func (r *recvFailStream) Recv() (*nodeattestor.AttestRequest, error) {
	return nil, errors.New("failed to recv from stream")
}

type sendFailStream struct {
	nodeattestor.Attest_PluginStream

	req *nodeattestor.AttestRequest
}

func (s *sendFailStream) Recv() (*nodeattestor.AttestRequest, error) {
	return s.req, nil
}

func (s *sendFailStream) Send(*nodeattestor.AttestResponse) error {
	return errors.New("failed to send to stream")
}

// get a DescribeInstancesOutput with essential structs created, but no values
// (device index and root device type) filled out
func getDefaultDescribeInstancesOutput() ec2.DescribeInstancesOutput {
	return ec2.DescribeInstancesOutput{
		Reservations: []*ec2.Reservation{
			&ec2.Reservation{
				Instances: []*ec2.Instance{
					&ec2.Instance{
						NetworkInterfaces: []*ec2.InstanceNetworkInterface{
							&ec2.InstanceNetworkInterface{
								Attachment: &ec2.InstanceNetworkInterfaceAttachment{},
							},
						},
					},
				},
			},
		},
	}
}

func (s *IIDAttestorSuite) attest(req *nodeattestor.AttestRequest) (*nodeattestor.AttestResponse, error) {
	stream, err := s.p.Attest(context.Background())
	defer stream.CloseSend()
	s.Require().NoError(err)
	err = stream.Send(req)
	s.Require().NoError(err)
	return stream.Recv()
}

func (s *IIDAttestorSuite) requireErrorContains(err error, substring string) {
	s.Require().Error(err)
	s.Require().Contains(err.Error(), substring)
}

func (s *IIDAttestorSuite) buildIIDAttestationData(instanceID, accountID, region string) *aws.IIDAttestationData {
	// doc body
	doc := aws.InstanceIdentityDocument{
		AccountID:  accountID,
		InstanceID: instanceID,
		Region:     region,
	}
	docBytes, err := json.Marshal(doc)
	s.Require().NoError(err)

	// doc signature
	rng := rand.Reader
	docHash := sha256.Sum256(docBytes)
	sig, err := rsa.SignPKCS1v15(rng, s.rsaKey, crypto.SHA256, docHash[:])
	s.Require().NoError(err)

	return &aws.IIDAttestationData{
		Document:  string(docBytes),
		Signature: base64.StdEncoding.EncodeToString(sig),
	}
}

func (s *IIDAttestorSuite) buildDefaultIIDAttestationData() *aws.IIDAttestationData {
	return s.buildIIDAttestationData(testInstance, testAccount, testRegion)
}

func (s *IIDAttestorSuite) iidAttestationDataToBytes(data aws.IIDAttestationData) []byte {
	dataBytes, err := json.Marshal(data)
	s.Require().NoError(err)
	return dataBytes
}
