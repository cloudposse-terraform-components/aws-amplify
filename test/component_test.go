package test

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/amplify"
	"github.com/aws/aws-sdk-go-v2/service/amplify/types"
	amplify_types "github.com/aws/aws-sdk-go-v2/service/amplify/types"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	route53_types "github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/cloudposse/test-helpers/pkg/atmos"
	helper "github.com/cloudposse/test-helpers/pkg/atmos/aws-component-helper"
	"github.com/gruntwork-io/terratest/modules/aws"
	http_helper "github.com/gruntwork-io/terratest/modules/http-helper"
	"github.com/gruntwork-io/terratest/modules/retry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComponent(t *testing.T) {
	// Define the AWS region to use for the tests
	awsRegion := "us-east-2"

	// Initialize the test fixture
	fixture := helper.NewFixture(t, "../", awsRegion, "test/fixtures")

	// Ensure teardown is executed after the test
	defer fixture.TearDown()
	fixture.SetUp(&atmos.Options{})

	// Define the test suite
	fixture.Suite("default", func(t *testing.T, suite *helper.Suite) {
		// Setup phase: Create DNS zones for testing
		suite.Setup(t, func(t *testing.T, atm *helper.Atmos) {
			basicDomain := "components.cptest.test-automation.app"

			// Deploy the delegated DNS zone
			inputs := map[string]interface{}{
				"zone_config": []map[string]interface{}{
					{
						"subdomain": suite.GetRandomIdentifier(),
						"zone_name": basicDomain,
					},
				},
			}
			atm.GetAndDeploy("dns-delegated", "default-test", inputs)
		})

		// Teardown phase: Destroy the DNS zones created during setup
		suite.TearDown(t, func(t *testing.T, atm *helper.Atmos) {
			// Deploy the delegated DNS zone
			inputs := map[string]interface{}{
				"zone_config": []map[string]interface{}{
					{
						"subdomain": suite.GetRandomIdentifier(),
						"zone_name": "components.cptest.test-automation.app",
					},
				},
			}
			atm.GetAndDestroy("dns-delegated", "default-test", inputs)
		})

		// Test phase: Validate the functionality of the ALB component
		suite.Test(t, "basic", func(t *testing.T, atm *helper.Atmos) {
			inputs := map[string]interface{}{}
			defer atm.GetAndDestroy("amplify/basic", "default-test", inputs)
			component := atm.GetAndDeploy("amplify/basic", "default-test", inputs)
			assert.NotNil(t, component)

			dnsDelegatedComponent := helper.NewAtmosComponent("dns-delegated", "default-test", map[string]interface{}{})
			delegatedDomain := atm.Output(dnsDelegatedComponent, "default_domain_name")

			name := atm.Output(component, "name")
			assert.NotEmpty(t, name)

			arn := atm.Output(component, "arn")
			assert.NotEmpty(t, arn)

			id := strings.Split(arn, "/")[1]

			defaultDomain := atm.Output(component, "default_domain")
			assert.NotEmpty(t, defaultDomain)

			backendEnvironments := map[string]interface{}{}
			atm.OutputStruct(component, "backend_environments", &backendEnvironments)
			assert.Equal(t, map[string]interface{}{}, backendEnvironments)

			branchNames := atm.OutputList(component, "branch_names")
			assert.Equal(t, "develop", branchNames[0])
			assert.Equal(t, "main", branchNames[1])

			webhooks := map[string]interface{}{}
			atm.OutputStruct(component, "webhooks", &webhooks)
			assert.Equal(t, map[string]interface{}{}, webhooks)

			domainAssociationArn := atm.Output(component, "domain_association_arn")
			assert.NotEmpty(t, domainAssociationArn)

			certificateVerificationDNSRecord := atm.Output(component, "domain_association_certificate_verification_dns_record")
			assert.NotEmpty(t, certificateVerificationDNSRecord)

			certificateVerificationDNSRecordArray := strings.Split(certificateVerificationDNSRecord, " ")
			certificateVerificationDNSRecordName := strings.TrimRight(certificateVerificationDNSRecordArray[0], ".")
			certificateVerificationDNSRecordType := certificateVerificationDNSRecordArray[1]

			delegatedZoneId := atm.Output(dnsDelegatedComponent, "default_dns_zone_id")

			defer func() {
				dnsRecord, err := aws.GetRoute53RecordE(t, delegatedZoneId, certificateVerificationDNSRecordName, certificateVerificationDNSRecordType, awsRegion)
				if err != nil {
					t.Logf("Failed to get DNS record %s: %s", certificateVerificationDNSRecord, err)
					return
				}
				client := aws.NewRoute53Client(t, awsRegion)
				changeResourceRecordsSetInput := &route53.ChangeResourceRecordSetsInput{
					HostedZoneId: &delegatedZoneId,
					ChangeBatch: &route53_types.ChangeBatch{
						Changes: []route53_types.Change{
							{
								Action:            route53_types.ChangeActionDelete,
								ResourceRecordSet: dnsRecord,
							},
						},
					},
				}
				_, err = client.ChangeResourceRecordSets(context.Background(), changeResourceRecordsSetInput)
				if err != nil {
					t.Logf("Failed to delete DNS record %s: %s", certificateVerificationDNSRecordName, err)
				}
			}()

			subDomain := atm.OutputList(component, "sub_domains")
			assert.Equal(t, 2, len(subDomain))

			client := NewAmplifyClient(t, awsRegion)
			apps, err := client.GetApp(context.Background(), &amplify.GetAppInput{
				AppId: &id,
			})

			assert.NoError(t, err)
			assert.Equal(t, arn, *apps.App.AppArn)
			assert.Equal(t, defaultDomain, *apps.App.DefaultDomain)

			var wg sync.WaitGroup

			for _, branchName := range branchNames {
				wg.Add(1)

				go func() {
					defer wg.Done()
					jobId := StartDeploymentJob(t, client, &id, &branchName)
					_, err = retry.DoWithRetryE(t, fmt.Sprintf("Wait deployment %s", branchName), 30, 10*time.Second, func() (string, error) {
						job, err := client.GetJob(context.Background(), &amplify.GetJobInput{
							AppId:      &id,
							BranchName: &branchName,
							JobId:      jobId,
						})
						if err == nil && job.Job.Summary.Status != amplify_types.JobStatusSucceed {
							return "", fmt.Errorf("Job %s have status %s", *jobId, job.Job.Summary.Status)
						}

						return "", err
					})
					assert.NoError(t, err)
				}()
			}

			wg.Wait()

			branchDevelop, err := client.GetDomainAssociation(context.Background(), &amplify.GetDomainAssociationInput{
				AppId:      &id,
				DomainName: &delegatedDomain,
			})
			assert.NoError(t, err)

			// Setup a TLS configuration to submit with the helper, a blank struct is acceptable
			tlsConfig := tls.Config{}

			// It can take a minute or so for the Instance to boot up, so retry a few times
			maxRetries := 30
			timeBetweenRetries := 5 * time.Second

			for _, subDomain := range branchDevelop.DomainAssociation.SubDomains {
				url := fmt.Sprintf("https://%s.%s", *subDomain.SubDomainSetting.Prefix, delegatedDomain)
				options := http_helper.HttpGetOptions{Url: url, TlsConfig: &tlsConfig, Timeout: 10}
				// Verify that we get back a 200 OK with the expected instanceText
				validateResponse := func(statusCode int, body string) bool {
					return statusCode == 200 && strings.Contains(body, "Web site created using create-react-app")
				}
				err := HttpGetWithRetryWithOptionsE(t, options, validateResponse, maxRetries, timeBetweenRetries)
				assert.NoError(t, err)
			}
		})
	})
}

func StartDeploymentJob(t *testing.T, client *amplify.Client, id *string, branchName *string) *string {
	branch, err := client.GetBranch(context.Background(), &amplify.GetBranchInput{
		AppId:      id,
		BranchName: branchName,
	})
	require.NoError(t, err)

	var jobType types.JobType
	if branch.Branch.ActiveJobId == nil {
		jobType = types.JobTypeRelease
	} else {
		jobType = types.JobTypeRetry
	}
	jobStart, err := client.StartJob(context.Background(), &amplify.StartJobInput{
		AppId:      id,
		BranchName: branchName,
		JobId:      branch.Branch.ActiveJobId,
		JobType:    jobType,
	})
	assert.NoError(t, err)
	return jobStart.JobSummary.JobId
}

func HttpGetWithRetryWithOptionsE(t *testing.T, options http_helper.HttpGetOptions, validateResponse func(int, string) bool, retries int, sleepBetweenRetries time.Duration) error {
	_, err := retry.DoWithRetryE(t, fmt.Sprintf("HTTP GET to URL %s", options.Url), retries, sleepBetweenRetries, func() (string, error) {
		return "", http_helper.HttpGetWithCustomValidationWithOptionsE(t, options, validateResponse)
	})

	return err
}

func NewAmplifyClient(t *testing.T, region string) *amplify.Client {
	client, err := NewAmplifyClientE(t, region)
	require.NoError(t, err)

	return client
}

func NewAmplifyClientE(t *testing.T, region string) (*amplify.Client, error) {
	sess, err := aws.NewAuthenticatedSession(region)
	if err != nil {
		return nil, err
	}
	return amplify.NewFromConfig(*sess), nil
}
