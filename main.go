package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sqs"
	sparta "github.com/mweagle/Sparta"
	spartaAWS "github.com/mweagle/Sparta/aws"
	spartaCF "github.com/mweagle/Sparta/aws/cloudformation"
	spartaIAM "github.com/mweagle/Sparta/aws/iam"
	spartaDocker "github.com/mweagle/Sparta/docker"
	gocf "github.com/mweagle/go-cloudformation"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

const userDataScriptTemplate = `#!/bin/bash -xe
yum install -y aws-cfn-bootstrap
/opt/aws/bin/cfn-init -v --stack { "Ref" : "AWS::StackName" } --resource {{ .LaunchConfigurationName }} --region { "Ref" : "AWS::Region" }
/opt/aws/bin/cfn-signal -e $? --stack { "Ref" : "AWS::StackName" } --resource {{ .AutoScalingGroupName }} --region { "Ref" : "AWS::Region" }
`

const cfnHUPScriptTemplate = `[main]
stack={ "Ref" : "AWS::StackId" }
region={ "Ref" : "AWS::Region" }`

const cfnAutoReloaderScriptTemplate = `[cfn-auto-reloader-hook]
triggers=post.update
path=Resources.{{ .LaunchConfigurationName }}.Metadata.AWS::CloudFormation::Init
action=/opt/aws/bin/cfn-init -v --stack { "Ref" : "AWS::StackName" } --resource {{ .LaunchConfigurationName }} --region { "Ref" : "AWS::Region" }
runas=root`

const ecrRepositoryName = "spartadocker"
const sqsQueueURLEnvVar = "SQS_QUEUE_URL"
const sqsQueueNameEnvVar = "SQS_QUEUE_NAME"

// Shared SQS resource to which the Lambda function will push
// and the ECS pool will pull
var sqsResourceName = stableCloudFormationResourceName("WorkerQueue")

// SSHKeyName is the SSH KeyName to use when provisioning new EC2 instance
var SSHKeyName string

func stableCloudFormationResourceName(component string) string {
	return sparta.CloudFormationResourceName(component, component)
}

func convertToTemplateExpression(templateData string,
	userDataProps map[string]interface{}) (*gocf.StringExpr,
	error) {
	templateReader := strings.NewReader(templateData)
	return spartaCF.ConvertToTemplateExpression(templateReader, userDataProps)
}

// Standard AWS λ function
func helloWorldLambda(ctx context.Context) (string, error) {
	logger, _ := ctx.Value(sparta.ContextKeyLogger).(*logrus.Logger)
	configuration, configurationErr := sparta.Discover()

	logger.WithFields(logrus.Fields{
		"Discovery": configuration,
		"Error":     configurationErr,
	}).Info("Custom resource request")

	sqsResource := configuration.Resources[sqsResourceName]
	queueName := sqsResource.Properties["QueueName"]
	if "" != queueName {
		// Push a message onto the queue
		sess, sessionError := session.NewSession()
		if sessionError != nil {
			return "",
				fmt.Errorf("Failed to create new AWS session: %s", sessionError)
		}
		sqsSvc := sqs.New(sess)
		queueURL := fmt.Sprintf("https://sqs.%s.amazonaws.com/%s", *sess.Config.Region, queueName)
		sendMessageInput := sqs.SendMessageInput{
			MessageBody: aws.String("Hello from Sparta!"),
			QueueUrl:    aws.String(queueURL),
		}
		sendMessageResponse, sendMessageErr := sqsSvc.SendMessage(&sendMessageInput)
		if nil != sendMessageErr {
			logger.WithFields(logrus.Fields{
				"SendError": sendMessageErr,
			}).Warn("Failed to send message")
		} else {
			return fmt.Sprintf("Message sent to SQS: %s", sendMessageResponse), nil
		}
	} else {
		return "", fmt.Errorf("Failed to find SQS queue name: %s", sqsResourceName)
	}
	return "", fmt.Errorf("Failed to find Queue name")
}

// More information on ECS
// http://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/quickref-ecs.html
func helloWorldDecorator(serviceName string,
	lambdaResourceName string,
	lambdaResource gocf.LambdaFunction,
	resourceMetadata map[string]interface{},
	S3Bucket string,
	S3Key string,
	buildID string,
	cfTemplate *gocf.Template,
	context map[string]interface{},
	logger *logrus.Logger) error {

	if len(SSHKeyName) <= 0 {
		return fmt.Errorf("Please provide a -k/--key argument for the SSH keyname")
	}

	logger.WithFields(logrus.Fields{
		"DecoratorContext": context,
	}).Debug("Decorate template with ElasticContainerService info")

	// 1 - Setup the SQS that we'll publish to
	sqs := &gocf.SQSQueue{}
	cfTemplate.AddResource(sqsResourceName, sqs)

	////////////////////////////////////////////////////////////////////////////
	// Setup the ECS cluster
	// Based on :
	// https://stelligent.com/2016/05/26/automating-ecs-provisioning-in-cloudformation-part-1/

	// Setup all the resource names
	ecsClusterName := stableCloudFormationResourceName("ECSCluster")
	ecsServiceName := stableCloudFormationResourceName("ECSService")
	ecsTaskDefinitionName := stableCloudFormationResourceName("ECSTaskDefinition")
	asgName := stableCloudFormationResourceName("ECSAutoScalingGroup")
	asgLaunchConfigName := stableCloudFormationResourceName("ECSAutoScalingLaunchConfig")
	iamInstanceProfileName := stableCloudFormationResourceName("IAMInstanceProfile")
	iamEC2RoleName := stableCloudFormationResourceName("EC2IAMRoleName")
	ecsLogsGroupName := stableCloudFormationResourceName("ECSLogGroupName")

	// Single property map for all userdata expansions
	configScriptProps := map[string]interface{}{
		"ECSClusterName":          ecsClusterName,
		"LaunchConfigurationName": asgLaunchConfigName,
		"InstanceProfileName":     iamInstanceProfileName,
		"AutoScalingGroupName":    asgName,
	}

	userDataScript, userDataScriptErr :=
		convertToTemplateExpression(userDataScriptTemplate, configScriptProps)
	if nil != userDataScriptErr {
		return userDataScriptErr
	}

	cfnHUPScript, cfnHUPScriptErr := convertToTemplateExpression(cfnHUPScriptTemplate,
		configScriptProps)
	if nil != cfnHUPScriptErr {
		return cfnHUPScriptErr
	}
	cfnAutoReloadScript, cfnAutoReloadScriptErr := convertToTemplateExpression(cfnAutoReloaderScriptTemplate,
		configScriptProps)
	if nil != cfnAutoReloadScriptErr {
		return cfnAutoReloadScriptErr
	}

	// 1 - ECS Cluster
	ecsCluster := gocf.ECSCluster{}
	cfTemplate.AddResource(ecsClusterName, ecsCluster)

	// 2 - ECS Service
	ecsService := gocf.ECSService{
		Cluster:      gocf.Ref(ecsClusterName).String(),
		DesiredCount: gocf.Integer(1),
		DeploymentConfiguration: &gocf.ECSServiceDeploymentConfiguration{
			MaximumPercent:        gocf.Integer(100),
			MinimumHealthyPercent: gocf.Integer(0),
		},
		TaskDefinition: gocf.Ref(ecsTaskDefinitionName).String(),
	}
	ecsServiceRes := cfTemplate.AddResource(ecsServiceName, ecsService)
	ecsServiceRes.DependsOn = append(ecsServiceRes.DependsOn, asgName)

	// 3a - LogGroups
	// Ref: http://docs.aws.amazon.com/AmazonECS/latest/developerguide/using_awslogs.html
	logGroup := gocf.LogsLogGroup{
		RetentionInDays: gocf.Integer(1),
	}
	logGroupRes := cfTemplate.AddResource(ecsLogsGroupName, logGroup)
	logGroupRes.DeletionPolicy = "Retain"

	// 3b - Task Definition
	ecsTaskDefinition := gocf.ECSTaskDefinition{
		ContainerDefinitions: &gocf.ECSTaskDefinitionContainerDefinitionList{
			gocf.ECSTaskDefinitionContainerDefinition{
				Name:      gocf.String(serviceName),
				CPU:       gocf.Integer(10),
				Memory:    gocf.Integer(512),
				Essential: gocf.Bool(true),
				LogConfiguration: &gocf.ECSTaskDefinitionLogConfiguration{
					LogDriver: gocf.String("awslogs"),
					Options: map[string]interface{}{
						"awslogs-group":  gocf.Ref(ecsLogsGroupName).String(),
						"awslogs-region": gocf.Ref("AWS::Region").String(),
					},
				},
				Image: gocf.String(context["URL"].(string)),
				PortMappings: &gocf.ECSTaskDefinitionPortMappingList{
					gocf.ECSTaskDefinitionPortMapping{
						ContainerPort: gocf.Integer(9999),
					},
				},
				Environment: &gocf.ECSTaskDefinitionKeyValuePairList{
					gocf.ECSTaskDefinitionKeyValuePair{
						Name:  gocf.String("AWS_REGION"),
						Value: gocf.Ref("AWS::Region").String(),
					},
					gocf.ECSTaskDefinitionKeyValuePair{
						Name:  gocf.String(sqsQueueURLEnvVar),
						Value: gocf.Ref(sqsResourceName).String(),
					},
					gocf.ECSTaskDefinitionKeyValuePair{
						Name:  gocf.String(sqsQueueNameEnvVar),
						Value: gocf.GetAtt(sqsResourceName, "QueueName").String(),
					},
				},
			},
		},
	}
	cfTemplate.AddResource(ecsTaskDefinitionName, ecsTaskDefinition)

	// 4 - AutoScaling
	// Mapping from:
	// http://docs.aws.amazon.com/AmazonECS/latest/developerguide/ecs-optimized_AMI.html
	asg := gocf.AutoScalingAutoScalingGroup{
		LaunchConfigurationName: gocf.Ref(asgLaunchConfigName).String(),
		MinSize:                 gocf.String("1"),
		MaxSize:                 gocf.String("1"),
		DesiredCapacity:         gocf.String("1"),
		AvailabilityZones:       gocf.GetAZs(gocf.String("")),
		Tags: &gocf.AutoScalingAutoScalingGroupTagPropertyList{
			gocf.AutoScalingAutoScalingGroupTagProperty{
				Key:               gocf.String("Name"),
				Value:             gocf.String(fmt.Sprintf("%s ECS Node", serviceName)),
				PropagateAtLaunch: gocf.Bool(true),
			},
		},
	}
	asgRes := cfTemplate.AddResource(asgName, asg)
	asgRes.CreationPolicy = &gocf.CreationPolicy{
		ResourceSignal: &gocf.CreationPolicyResourceSignal{
			Timeout: gocf.String("PT15M"),
		},
	}
	asgRes.UpdatePolicy = &gocf.UpdatePolicy{
		AutoScalingRollingUpdate: &gocf.UpdatePolicyAutoScalingRollingUpdate{
			MinInstancesInService: gocf.Integer(1),
			MaxBatchSize:          gocf.Integer(1),
			PauseTime:             gocf.String("PT15M"),
			WaitOnResourceSignals: gocf.Bool(true),
		},
	}

	// 5 - LaunchConfiguration
	cfTemplate.Mappings["AWSRegionToAMI"] = &gocf.Mapping{
		"us-east-1":      {"AMIID": "ami-52cd5445"},
		"us-west-1":      {"AMIID": "ami-efa1e28f"},
		"us-west-2":      {"AMIID": "ami-a426edc4"},
		"eu-west-1":      {"AMIID": "ami-7b244e08"},
		"eu-central-1":   {"AMIID": "ami-721aec1d"},
		"ap-northeast-1": {"AMIID": "ami-058a4964"},
		"ap-southeast-1": {"AMIID": "ami-0d9f466e"},
		"ap-southeast-2": {"AMIID": "ami-7df2c61e"},
	}

	asgLaunchConfiguration := gocf.AutoScalingLaunchConfiguration{
		ImageID:            gocf.FindInMap("AWSRegionToAMI", gocf.Ref("AWS::Region").String(), gocf.String("AMIID")).String(),
		InstanceType:       gocf.String("t2.micro"),
		IamInstanceProfile: gocf.Ref(iamInstanceProfileName).String(),
		KeyName:            gocf.String(SSHKeyName),
		UserData:           gocf.Base64(userDataScript),
	}
	asgLaunchConfigurationRes := cfTemplate.AddResource(asgLaunchConfigName,
		asgLaunchConfiguration)

	asgLaunchConfigurationRes.Metadata = map[string]interface{}{
		"AWS::CloudFormation::Init": sparta.ArbitraryJSONObject{
			"config": sparta.ArbitraryJSONObject{
				"commands": sparta.ArbitraryJSONObject{
					"01_add_instance_to_cluster": sparta.ArbitraryJSONObject{
						"command": gocf.Join("",
							gocf.String("#!/bin/bash\n"),
							gocf.String("echo ECS_CLUSTER="),
							gocf.Ref(ecsClusterName).String(),
							gocf.String(" >> /etc/ecs/ecs.config"),
						),
					},
				},
				"files": sparta.ArbitraryJSONObject{
					"/etc/cfn/cfn-hup.conf": sparta.ArbitraryJSONObject{
						"content": cfnHUPScript,
						"mode":    "000400",
						"owner":   "root",
						"group":   "root",
					},
					"/etc/cfn/hooks.d/cfn-auto-reloader.conf": sparta.ArbitraryJSONObject{
						"content": cfnAutoReloadScript,
						"mode":    "000400",
						"owner":   "root",
						"group":   "root",
					},
				},
				"services": sparta.ArbitraryJSONObject{
					"sysvinit": sparta.ArbitraryJSONObject{
						"cfn-hup": sparta.ArbitraryJSONObject{
							"enabled":       "true",
							"ensureRunning": "true",
							"files": []string{
								"/etc/cfn/cfn-hup.conf",
								"/etc/cfn/hooks.d/cfn-auto-reloader.conf"},
						},
					},
				},
			},
		},
	}

	// 6 - InstanceProfile
	instanceProfileRes := cfTemplate.AddResource(iamInstanceProfileName,
		&gocf.IAMInstanceProfile{
			Path:  gocf.String("/"),
			Roles: gocf.StringList(gocf.Ref(iamEC2RoleName)),
		})
	instanceProfileRes.DependsOn = append(instanceProfileRes.DependsOn, iamEC2RoleName)

	// 7 - IAM Role
	ec2IAMStatements := sparta.CommonIAMStatements.Core
	// TODO: POLA
	ec2IAMStatements = append(ec2IAMStatements, spartaIAM.PolicyStatement{
		Action: []string{"ecr:Get*",
			"ecs:CreateCluster",
			"ecs:RegisterContainerInstance",
			"ecs:DeregisterContainerInstance",
			"ecs:DiscoverPollEndpoint",
			"ecs:Submit*",
			"ecs:Poll"},
		Effect:   "Allow",
		Resource: gocf.String("*"),
	})
	ec2IAMStatements = append(ec2IAMStatements, spartaIAM.PolicyStatement{
		Action: []string{"sqs:ChangeMessageVisibility",
			"sqs:DeleteMessage",
			"sqs:GetQueueAttributes",
			"sqs:ReceiveMessage"},
		Effect:   "Allow",
		Resource: gocf.GetAtt(sqsResourceName, "Arn").String(),
	})

	ec2IAMStatements = append(ec2IAMStatements, spartaIAM.PolicyStatement{
		Action: []string{
			"ecr:GetAuthorizationToken",
			"ecr:BatchCheckLayerAvailability",
			"ecr:GetDownloadUrlForLayer",
			"ecr:GetRepositoryPolicy",
			"ecr:DescribeRepositories",
			"ecr:ListImages",
			"ecr:BatchGetImage"},
		Effect: "Allow",
		Resource: gocf.Join("",
			gocf.String("arn:aws:ecr:"),
			gocf.Ref("AWS::Region"),
			gocf.String(":"),
			gocf.Ref("AWS::AccountId"),
			gocf.String(":repository/"),
			gocf.String(ecrRepositoryName)),
	})
	iamPolicyList := gocf.IAMRolePolicyList{}
	iamPolicyList = append(iamPolicyList,
		gocf.IAMRolePolicy{
			PolicyDocument: sparta.ArbitraryJSONObject{
				"Version":   "2012-10-17",
				"Statement": ec2IAMStatements,
			},
			PolicyName: gocf.String("EBSQSAccess"),
		},
	)
	iamEC2Role := &gocf.IAMRole{
		AssumeRolePolicyDocument: sparta.AssumePolicyDocument,
		Policies:                 &iamPolicyList,
	}
	cfTemplate.AddResource(iamEC2RoleName, iamEC2Role)
	return nil
}

// BuildDockerImageHook workflow hook to build the Docker image
func BuildDockerImageHook(context map[string]interface{},
	serviceName string,
	S3Bucket string,
	buildID string,
	awsSession *session.Session,
	noop bool,
	logger *logrus.Logger) error {

	dockerServiceName := strings.ToLower(serviceName)
	dockerTags := make(map[string]string, 0)
	dockerTags[dockerServiceName] = buildID

	// Always build the image
	buildErr := spartaDocker.BuildDockerImage(serviceName,
		"",
		dockerTags,
		logger)
	if nil != buildErr {
		return buildErr
	}
	var ecrURL string
	if !noop {
		// Push the image to ECR & store the URL s.t. we can properly annotate
		// the CloudFormation template
		localTag := fmt.Sprintf("%s:%s", dockerServiceName, buildID)
		ecrURLPush, pushImageErr := spartaDocker.PushDockerImageToECR(localTag,
			ecrRepositoryName,
			awsSession,
			logger)
		if nil != pushImageErr {
			return pushImageErr
		}
		ecrURL = ecrURLPush
		logger.WithFields(logrus.Fields{
			"ECRUrl":    ecrURL,
			"PushError": pushImageErr,
		}).Info("Docker image pushed")
	} else {
		ecrURL = fmt.Sprintf("https://123412341234.dkr.ecr.aws-region.amazonaws.com/%s", serviceName)
	}
	// Save the URL
	context["URL"] = ecrURL

	return nil
}

func sqsListener(logger *logrus.Logger) error {
	// Get the SQS queuename from the environment
	queueURL := os.Getenv(sqsQueueURLEnvVar)
	queueName := os.Getenv(sqsQueueNameEnvVar)
	logger.WithFields(logrus.Fields{
		"URL":  queueURL,
		"Name": queueName,
	}).Info("SQS queue information")

	// Setup a loop to process the message
	sess := spartaAWS.NewSession(logger)
	sqsSvc := sqs.New(sess)

	sqsRequestParams := &sqs.ReceiveMessageInput{
		QueueUrl: aws.String(queueURL),
		AttributeNames: []*string{
			aws.String(queueName),
		},
		MaxNumberOfMessages: aws.Int64(1),
		MessageAttributeNames: []*string{
			aws.String("MessageAttributeName"),
		},
		VisibilityTimeout: aws.Int64(1),
		WaitTimeSeconds:   aws.Int64(20),
	}

	for {
		sqsOutput, sqsOutputErr := sqsSvc.ReceiveMessage(sqsRequestParams)
		if nil == sqsOutputErr {
			if len(sqsOutput.Messages) != 0 {
				logger.WithFields(logrus.Fields{
					"Messages": *sqsOutput,
				}).Info("SQS message")
				// Delete them all
				for _, eachMessage := range sqsOutput.Messages {
					sqsDeleteRequest := &sqs.DeleteMessageInput{
						QueueUrl:      aws.String(queueURL),
						ReceiptHandle: eachMessage.ReceiptHandle,
					}
					// Delete it...
					sqsSvc.DeleteMessage(sqsDeleteRequest)
				}
			}
		} else {
			logger.WithFields(logrus.Fields{
				"Error": sqsOutputErr,
			}).Warn("Failed to receive message")
		}
	}
}

func main() {

	// Custom command to startup a simple HelloWorld HTTP server
	sqsWorkerCommand := &cobra.Command{
		Use:   "sqsWorker",
		Short: "Sample SQS Worker processor",
		Long:  fmt.Sprintf("Sample SQS listener"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return sqsListener(sparta.OptionsGlobal.Logger)
		},
	}
	sparta.CommandLineOptions.Root.AddCommand(sqsWorkerCommand)

	// And add the SSHKeyName option to the provision step
	sparta.CommandLineOptions.Provision.Flags().StringVarP(&SSHKeyName,
		"key",
		"k",
		"",
		"SSH Key Name to use for EC2 instances")

	// Sparta workflow hooks
	workflowHooks := sparta.WorkflowHooks{
		PostBuilds: []sparta.WorkflowHookHandler{
			sparta.WorkflowHookFunc(BuildDockerImageHook),
		},
	}

	// Setup an IAM role that allows the lambda function to send a message
	// to the queue.
	iamPolicy := sparta.IAMRoleDefinition{
		Privileges: []sparta.IAMRolePrivilege{
			sparta.IAMRolePrivilege{
				Actions: []string{
					"sqs:SendMessage"},
				Resource: gocf.GetAtt(sqsResourceName, "Arn").String(),
			},
		},
	}

	// The actual lambda functions
	lambdaFn := sparta.HandleAWSLambda("Hello World",
		helloWorldLambda,
		iamPolicy)
	lambdaFn.Decorators = []sparta.TemplateDecoratorHandler{
		sparta.TemplateDecoratorHookFunc(helloWorldDecorator),
	}
	lambdaFn.DependsOn = []string{sqsResourceName}

	var lambdaFunctions []*sparta.LambdaAWSInfo
	lambdaFunctions = append(lambdaFunctions, lambdaFn)
	stackName := spartaCF.UserScopedStackName("SpartaDocker")
	err := sparta.MainEx(stackName,
		fmt.Sprintf("Test Docker deployment"),
		lambdaFunctions,
		nil,
		nil,
		&workflowHooks,
		false)
	if err != nil {
		os.Exit(1)
	}
}
