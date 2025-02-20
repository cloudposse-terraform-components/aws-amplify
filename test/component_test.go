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
	awshelper "github.com/cloudposse/test-helpers/pkg/aws"
	amplify_types "github.com/aws/aws-sdk-go-v2/service/amplify/types"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	route53_types "github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/cloudposse/test-helpers/pkg/atmos"
	helper "github.com/cloudposse/test-helpers/pkg/atmos/component-helper"
	"github.com/gruntwork-io/terratest/modules/aws"
	http_helper "github.com/gruntwork-io/terratest/modules/http-helper"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/gruntwork-io/terratest/modules/retry"
	"github.com/stretchr/testify/assert"
)

type ComponentSuite struct {
	helper.TestSuite
}

func (s *ComponentSuite) TestBasic() {
	const component = "amplify/basic"
	const stack = "default-test"
	const awsRegion = "us-east-2"

	hostnamePrefix := strings.ToLower(random.UniqueId())
	inputs := map[string]interface{}{
		"domain_config": map[string]interface{}{
			"sub_domain": []map[string]interface{}{
				{
					"prefix":      fmt.Sprintf("%s-%s", hostnamePrefix, "example-prod"),
					"branch_name": "main",
				},
				{
					"prefix":      fmt.Sprintf("%s-%s", hostnamePrefix, "example-dev"),
					"branch_name": "develop",
				},
			},
		},
	}
	defer s.DestroyAtmosComponent(s.T(), component, stack, &inputs)
	options, _ := s.DeployAtmosComponent(s.T(), component, stack, &inputs)
	assert.NotNil(s.T(), options)

	dnsDelegatedOptions := s.GetAtmosOptions("dns-delegated", stack, nil)
	delegatedDomain := atmos.Output(s.T(), dnsDelegatedOptions, "default_domain_name")

	name := atmos.Output(s.T(), options, "name")
	assert.NotEmpty(s.T(), name)

	arn := atmos.Output(s.T(), options, "arn")
	assert.NotEmpty(s.T(), arn)

	id := strings.Split(arn, "/")[1]

	defaultDomain := atmos.Output(s.T(), options, "default_domain")
	assert.NotEmpty(s.T(), defaultDomain)

	backendEnvironments := map[string]interface{}{}
	atmos.OutputStruct(s.T(), options, "backend_environments", &backendEnvironments)
	assert.Equal(s.T(), map[string]interface{}{}, backendEnvironments)

	branchNames := atmos.OutputList(s.T(), options, "branch_names")
	assert.Equal(s.T(), "develop", branchNames[0])
	assert.Equal(s.T(), "main", branchNames[1])

	webhooks := map[string]interface{}{}
	atmos.OutputStruct(s.T(), options, "webhooks", &webhooks)
	assert.Equal(s.T(), map[string]interface{}{}, webhooks)

	domainAssociationArn := atmos.Output(s.T(), options, "domain_association_arn")
	assert.NotEmpty(s.T(), domainAssociationArn)

	certificateVerificationDNSRecord := atmos.Output(s.T(), options, "domain_association_certificate_verification_dns_record")
	assert.NotEmpty(s.T(), certificateVerificationDNSRecord)

	certificateVerificationDNSRecordArray := strings.Split(certificateVerificationDNSRecord, " ")
	certificateVerificationDNSRecordName := strings.TrimRight(certificateVerificationDNSRecordArray[0], ".")
	certificateVerificationDNSRecordType := certificateVerificationDNSRecordArray[1]

	delegatedZoneId := atmos.Output(s.T(), dnsDelegatedOptions, "default_dns_zone_id")

	defer func() {
		dnsRecord, err := aws.GetRoute53RecordE(s.T(), delegatedZoneId, certificateVerificationDNSRecordName, certificateVerificationDNSRecordType, awsRegion)
		if err != nil {
			s.T().Logf("Failed to get DNS record %s: %s", certificateVerificationDNSRecord, err)
			return
		}
		client := aws.NewRoute53Client(s.T(), awsRegion)
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
			s.T().Logf("Failed to delete DNS record %s: %s", certificateVerificationDNSRecordName, err)
		}
	}()

	subDomain := atmos.OutputList(s.T(), options, "sub_domains")
	assert.Equal(s.T(), 2, len(subDomain))

	client := awshelper.NewAmplifyClient(s.T(), awsRegion)
	apps, err := client.GetApp(context.Background(), &amplify.GetAppInput{
		AppId: &id,
	})

	assert.NoError(s.T(), err)
	assert.Equal(s.T(), arn, *apps.App.AppArn)
	assert.Equal(s.T(), defaultDomain, *apps.App.DefaultDomain)

	var wg sync.WaitGroup

	for _, branchName := range branchNames {
		wg.Add(1)

		go func(branchName string) {
			defer wg.Done()
			jobId := awshelper.StartDeploymentJob(s.T(), context.Background(), client, &id, &branchName)
			_, err = retry.DoWithRetryE(s.T(), fmt.Sprintf("Wait deployment %s", branchName), 30, 10*time.Second, func() (string, error) {
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
			assert.NoError(s.T(), err)
		}(branchName)
	}

	wg.Wait()

	branchDevelop, err := client.GetDomainAssociation(context.Background(), &amplify.GetDomainAssociationInput{
		AppId:      &id,
		DomainName: &delegatedDomain,
	})
	assert.NoError(s.T(), err)

	tlsConfig := tls.Config{}

	maxRetries := 30
	timeBetweenRetries := 5 * time.Second

	for _, subDomain := range branchDevelop.DomainAssociation.SubDomains {
		url := fmt.Sprintf("https://%s.%s", *subDomain.SubDomainSetting.Prefix, delegatedDomain)
		options := http_helper.HttpGetOptions{Url: url, TlsConfig: &tlsConfig, Timeout: 10}
		validateResponse := func(statusCode int, body string) bool {
			return statusCode == 200 && strings.Contains(body, "Web site created using create-react-app")
		}
		err := HttpGetWithRetryWithOptionsE(s.T(), options, validateResponse, maxRetries, timeBetweenRetries)
		assert.NoError(s.T(), err)
	}

	s.DriftTest(component, stack, &inputs)
}

func (s *ComponentSuite) TestEnabledFlag() {
	const component = "amplify/disabled"
	const stack = "default-test"
	s.VerifyEnabledFlag(component, stack, nil)
}

func HttpGetWithRetryWithOptionsE(t *testing.T, options http_helper.HttpGetOptions, validateResponse func(int, string) bool, retries int, sleepBetweenRetries time.Duration) error {
	_, err := retry.DoWithRetryE(t, fmt.Sprintf("HTTP GET to URL %s", options.Url), retries, sleepBetweenRetries, func() (string, error) {
		return "", http_helper.HttpGetWithCustomValidationWithOptionsE(t, options, validateResponse)
	})

	return err
}

func TestRunSuite(t *testing.T) {
	suite := new(ComponentSuite)

	subdomain := strings.ToLower(random.UniqueId())
	inputs := map[string]interface{}{
		"zone_config": []map[string]interface{}{
			{
				"subdomain": subdomain,
				"zone_name": "components.cptest.test-automation.app",
			},
		},
	}
	suite.AddDependency(t, "dns-delegated", "default-test", &inputs)
	helper.Run(t, suite)
}
