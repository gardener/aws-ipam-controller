/*
 * SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package updater

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// AccessKeyID is a constant for the key in a cloud provider secret and backup secret that holds the AWS access key id.
	AccessKeyID = "accessKeyID"
	// SecretAccessKey is a constant for the key in a cloud provider secret and backup secret that holds the AWS secret access key.
	SecretAccessKey = "secretAccessKey"
	// InClusterConfig is a special name for the kubeconfig to use in-cluster client
	InClusterConfig = "inClusterConfig"
	// WorkloadIdentityTokenFile is a constant for the key in a cloud provider secret and backup secret that holds the path to a workload identity token.
	WorkloadIdentityTokenFile = "workloadIdentityTokenFile"
	// RoleARN is a constant for the key in a cloud provider secret and backup secret that holds ARN of a role that is to be assumed.
	RoleARN = "roleARN"
)

type Credentials struct {
	// AccessKey represents static credentials for authentication to AWS.
	// This field is mutually exclusive with WorkloadIdentity.
	AccessKey *AccessKey

	// WorkloadIdentity contains workload identity configuration.
	// This field is mutually exclusive with AccessKey.
	WorkloadIdentity *WorkloadIdentity
}

// AccessKey represents static credentials for authentication to AWS.
type AccessKey struct {
	// ID is the key ID used for access to AWS.
	ID string
	// Secret is the secret used for access to AWS.
	Secret string
}

// WorkloadIdentity contains workload identity configuration for authentication to AWS.
type WorkloadIdentity struct {
	// TokenRetriever a function that retrieves a token used for exchanging AWS credentials.
	TokenRetriever stscreds.IdentityTokenRetriever

	// RoleARN is the ARN of the role that will be assumed.
	RoleARN string
}

func LoadCredentials(controlKubeconfig, namespace, secretName string) (*Credentials, error) {
	var err error
	var config *rest.Config
	if controlKubeconfig == InClusterConfig || controlKubeconfig == "" {
		config, err = rest.InClusterConfig()
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", controlKubeconfig)
	}
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	secret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	return extractCredentials(secret)
}

func extractCredentials(secret *corev1.Secret) (*Credentials, error) {
	if secret.Data == nil {
		return nil, fmt.Errorf("secret does not contain any data")
	}

	if workloadIdentityTokenFile, ok := secret.Data[WorkloadIdentityTokenFile]; ok {
		if len(workloadIdentityTokenFile) == 0 {
			return nil, fmt.Errorf("workloadIdentityTokenFile must not be empty")
		}

		roleARN, ok := secret.Data[RoleARN]
		if !ok || len(roleARN) == 0 {
			return nil, fmt.Errorf("roleARN is required")
		}

		return &Credentials{
			WorkloadIdentity: &WorkloadIdentity{
				TokenRetriever: &fileTokenRetriever{
					fileName: string(workloadIdentityTokenFile),
				},
				RoleARN: string(roleARN),
			},
		}, nil
	}

	accessKeyID, err := getSecretDataValue(secret, AccessKeyID, nil, true)
	if err != nil {
		return nil, err
	}

	secretAccessKey, err := getSecretDataValue(secret, SecretAccessKey, nil, true)
	if err != nil {
		return nil, err
	}

	return &Credentials{
		AccessKey: &AccessKey{
			ID:     string(accessKeyID),
			Secret: string(secretAccessKey),
		},
	}, nil
}

func getSecretDataValue(secret *corev1.Secret, key string, altKey *string, required bool) ([]byte, error) {
	if value, ok := secret.Data[key]; ok {
		return value, nil
	}
	if altKey != nil {
		if value, ok := secret.Data[*altKey]; ok {
			return value, nil
		}
	}
	if required {
		if altKey != nil {
			return nil, fmt.Errorf("missing %q (or %q) field in secret", key, *altKey)
		}
		return nil, fmt.Errorf("missing %q field in secret", key)
	}
	return nil, nil
}

func NewAWSEC2V2(ctx context.Context, cred *Credentials, region string) (*ec2.Client, error) {
	var credentialsProvider aws.CredentialsProvider
	switch {
	case cred.AccessKey != nil:
		credentialsProvider = credentials.NewStaticCredentialsProvider(cred.AccessKey.ID, cred.AccessKey.Secret, "")
	case cred.WorkloadIdentity != nil:
		credentialsProvider = stscreds.NewWebIdentityRoleProvider(
			sts.NewFromConfig(aws.Config{Region: region}),
			cred.WorkloadIdentity.RoleARN,
			cred.WorkloadIdentity.TokenRetriever,
		)
	default:
		return nil, errors.New("credentials should either contain access key or workload identity config")
	}

	config, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithEC2IMDSEndpointMode(imds.EndpointModeStateIPv6),
		awsconfig.WithCredentialsProvider(aws.NewCredentialsCache(credentialsProvider)),
	)
	if err != nil {
		return nil, fmt.Errorf("error loading default AWS config: %v", err)
	}

	ec2Config := config.Copy()
	ec2Config.Region = region
	ec2Client := ec2.NewFromConfig(ec2Config)
	return ec2Client, err
}

type fileTokenRetriever struct {
	fileName string
}

var _ stscreds.IdentityTokenRetriever = (*fileTokenRetriever)(nil)

func (f *fileTokenRetriever) GetIdentityToken() ([]byte, error) {
	return os.ReadFile(f.fileName)
}
