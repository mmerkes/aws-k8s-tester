// Package alb implements ALB plugin that installs 2048.
package alb

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-k8s-tester/eks/elb"
	"github.com/aws/aws-k8s-tester/eksconfig"
	awscfn "github.com/aws/aws-k8s-tester/pkg/aws/cloudformation"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/cloudformation/cloudformationiface"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"
	"github.com/mitchellh/colorstring"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	v1beta1 "k8s.io/api/extensions/v1beta1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/utils/exec"
)

// Config defines ALB configuration.
type Config struct {
	Logger            *zap.Logger
	Stopc             chan struct{}
	Sig               chan os.Signal
	EKSConfig         *eksconfig.Config
	CloudFormationAPI cloudformationiface.CloudFormationAPI
	K8SClient         k8sClientSetGetter
	ELB2API           elbv2iface.ELBV2API
}

type k8sClientSetGetter interface {
	KubernetesClientSet() *clientset.Clientset
}

// Tester defines Job tester.
type Tester interface {
	// Create creates Job objects, and waits for completion.
	Create() error
	// Delete deletes all Job objects.
	Delete() error
}

// New creates a new Job tester.
func New(cfg Config) (Tester, error) {
	return &tester{cfg: cfg}, nil
}

type tester struct {
	cfg Config
}

const (
	albIngressControllerName = "alb-ingress-controller"

	albIngressControllerServiceAccountName      = "alb-ingress-controller-service-account"
	albIngressControllerServiceAccountNamespace = "kube-system"

	// cluster-wide role
	albIngressControllerRBACClusterRoleName      = "alb-ingress-controller-rbac-cluster-role"
	albIngressControllerRBACClusterRoleNamespace = "default"

	// cluster-wide role binding
	albIngressControllerRBACClusterRoleBindingName      = "alb-ingress-controller-rbac-cluster-role-binding"
	albIngressControllerRBACClusterRoleBindingNamespace = "default"

	albIngressControllerDeploymentName      = "alb-ingress-controller-deployment"
	albIngressControllerDeploymentNamespace = "kube-system"

	alb2048AppName        = "alb-2048"
	alb2048ImageName      = "alexwhen/docker-2048"
	alb2048DeploymentName = "alb-2048-deployment"
	alb2048ServiceName    = "alb-2048-service"
	alb2048IngressName    = "alb-2048-ingress"
)

// ALBImageName is the image name of ALB Ingress Controller.
// ref. https://github.com/kubernetes-sigs/aws-alb-ingress-controller/releases
const ALBImageName = "docker.io/amazon/aws-alb-ingress-controller:v1.1.5"

// https://docs.aws.amazon.com/eks/latest/userguide/alb-ingress.html
func (ts *tester) Create() error {
	if ts.cfg.EKSConfig.AddOnALB2048.Created {
		ts.cfg.Logger.Info("skipping create AddOnALB2048")
		return nil
	}

	ts.cfg.EKSConfig.AddOnALB2048.Created = true
	ts.cfg.EKSConfig.Sync()
	createStart := time.Now()
	defer func() {
		ts.cfg.EKSConfig.AddOnALB2048.CreateTook = time.Since(createStart)
		ts.cfg.EKSConfig.AddOnALB2048.CreateTookString = ts.cfg.EKSConfig.AddOnALB2048.CreateTook.String()
		ts.cfg.EKSConfig.Sync()
	}()

	if err := ts.createALBPolicy(); err != nil {
		return err
	}

	if err := ts.createNamespace(); err != nil {
		return err
	}

	if err := ts.createALBServiceAccount(); err != nil {
		return err
	}
	if err := ts.createALBRBACClusterRole(); err != nil {
		return err
	}
	if err := ts.createALBRBACClusterRoleBinding(); err != nil {
		return err
	}
	if err := ts.createALBDeployment(); err != nil {
		return err
	}
	if err := ts.waitDeploymentALB(); err != nil {
		return err
	}

	if err := ts.create2048Deployment(); err != nil {
		return err
	}
	if err := ts.waitDeployment2048(); err != nil {
		return err
	}
	if err := ts.create2048Service(); err != nil {
		return err
	}
	if err := ts.create2048Ingress(); err != nil {
		return err
	}

	return ts.cfg.EKSConfig.Sync()
}

func (ts *tester) Delete() error {
	if !ts.cfg.EKSConfig.AddOnALB2048.Created {
		ts.cfg.Logger.Info("skipping delete AddOnALB2048")
		return nil
	}

	deleteStart := time.Now()
	defer func() {
		ts.cfg.EKSConfig.AddOnALB2048.DeleteTook = time.Since(deleteStart)
		ts.cfg.EKSConfig.AddOnALB2048.DeleteTookString = ts.cfg.EKSConfig.AddOnALB2048.DeleteTook.String()
		ts.cfg.EKSConfig.Sync()
	}()

	var errs []string
	if err := ts.delete2048Ingress(); err != nil {
		errs = append(errs, fmt.Sprintf("failed to delete ALB 2048 Ingress (%v)", err))
	}
	ts.cfg.Logger.Info("wait for a minute after deleting 2048 Ingress")
	time.Sleep(time.Minute)

	if err := ts.delete2048Service(); err != nil {
		errs = append(errs, fmt.Sprintf("failed to delete ALB 2048 Service (%v)", err))
	}
	ts.cfg.Logger.Info("wait for a minute after deleting 2048 Service")
	time.Sleep(time.Minute)

	if err := ts.delete2048Deployment(); err != nil {
		errs = append(errs, fmt.Sprintf("failed to delete ALB 2048 Deployment (%v)", err))
	}
	time.Sleep(30 * time.Second)

	if err := ts.deleteALBDeployment(); err != nil {
		errs = append(errs, fmt.Sprintf("failed to delete ALB Ingress Controller Deployment (%v)", err))
	}
	ts.cfg.Logger.Info("wait for a minute after deleting ALB Deployment")
	time.Sleep(time.Minute)

	if err := ts.deleteALBRBACClusterRoleBinding(); err != nil {
		errs = append(errs, fmt.Sprintf("failed to delete ALB Ingress Controller RBAC (%v)", err))
	}
	time.Sleep(10 * time.Second)

	if err := ts.deleteALBRBACClusterRole(); err != nil {
		errs = append(errs, fmt.Sprintf("failed to delete ALB Ingress Controller RBAC (%v)", err))
	}
	time.Sleep(10 * time.Second)

	if err := ts.deleteALBServiceAccount(); err != nil {
		errs = append(errs, fmt.Sprintf("failed to delete ALB Ingress Controller ServiceAccount (%v)", err))
	}
	time.Sleep(10 * time.Second)

	if err := ts.deleteALBPolicy(); err != nil {
		errs = append(errs, fmt.Sprintf("failed to delete ALB Ingress Controller Policy (%v)", err))
	}
	ts.cfg.Logger.Info("wait for a minute after deleting ALB Policy")
	time.Sleep(time.Minute)

	if err := elb.DeleteELBv2(ts.cfg.Logger, ts.cfg.ELB2API, ts.cfg.EKSConfig.AddOnALB2048.ALBARN); err != nil {
		errs = append(errs, fmt.Sprintf("failed to delete ALB (%v)", err))
	}

	if err := ts.deleteNamespace(); err != nil {
		errs = append(errs, fmt.Sprintf("failed to delete ALB namespace (%v)", err))
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, ", "))
	}

	ts.cfg.EKSConfig.AddOnALB2048.Created = false
	return ts.cfg.EKSConfig.Sync()
}

func (ts *tester) createNamespace() error {
	ts.cfg.Logger.Info("creating namespace", zap.String("namespace", ts.cfg.EKSConfig.AddOnALB2048.Namespace))
	_, err := ts.cfg.K8SClient.KubernetesClientSet().
		CoreV1().
		Namespaces().
		Create(&v1.Namespace{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "Namespace",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: ts.cfg.EKSConfig.AddOnALB2048.Namespace,
				Labels: map[string]string{
					"name": ts.cfg.EKSConfig.AddOnALB2048.Namespace,
				},
			},
		})
	if err != nil {
		return err
	}
	ts.cfg.Logger.Info("created namespace", zap.String("namespace", ts.cfg.EKSConfig.AddOnALB2048.Namespace))
	return ts.cfg.EKSConfig.Sync()
}

func (ts *tester) deleteNamespace() error {
	ts.cfg.Logger.Info("deleting namespace", zap.String("namespace", ts.cfg.EKSConfig.AddOnALB2048.Namespace))
	foreground := metav1.DeletePropagationForeground
	err := ts.cfg.K8SClient.KubernetesClientSet().
		CoreV1().
		Namespaces().
		Delete(
			ts.cfg.EKSConfig.AddOnALB2048.Namespace,
			&metav1.DeleteOptions{
				GracePeriodSeconds: aws.Int64(0),
				PropagationPolicy:  &foreground,
			},
		)
	if err != nil {
		return err
	}
	ts.cfg.Logger.Info("deleted namespace", zap.String("namespace", ts.cfg.EKSConfig.AddOnALB2048.Namespace))
	return ts.cfg.EKSConfig.Sync()
}

// TemplateALBIngressControllerPolicy is the CloudFormation template for ALB Ingress Controller Policy.
// https://github.com/kubernetes-sigs/aws-alb-ingress-controller
// https://github.com/kubernetes-sigs/aws-alb-ingress-controller/blob/master/docs/examples/iam-policy.json
// https://docs.aws.amazon.com/eks/latest/userguide/alb-ingress.html
const TemplateALBIngressControllerPolicy = `
---
AWSTemplateFormatVersion: '2010-09-09'
Description: 'Amazon EKS ALB Ingress Controller Policy'

Parameters:

  ALBIngressControllerPolicyName:
    Description: The policy name for ALB Ingress Controller
    Type: String

  ManagedNodeGroupRoleName:
    Description: The name of the node instance role
    Type: String

Resources:

  ALBIngressControllerPolicy:
    Metadata:
      Comment: Minimal policy to allow worker node instance profile that allows the ALB Ingress Controller to make calls to AWS APIs on your behalf
    Type: AWS::IAM::Policy
    Properties:
      PolicyName: !Ref ALBIngressControllerPolicyName
      PolicyDocument:
        Version: '2012-10-17'
        Statement:
        - Effect: Allow
          Action:
          - acm:DescribeCertificate
          - acm:ListCertificates
          - acm:GetCertificate
          Resource: "*"
        - Effect: Allow
          Action:
          - ec2:AuthorizeSecurityGroupIngress
          - ec2:CreateSecurityGroup
          - ec2:CreateTags
          - ec2:DeleteTags
          - ec2:DeleteSecurityGroup
          - ec2:DescribeAccountAttributes
          - ec2:DescribeAddresses
          - ec2:DescribeInstances
          - ec2:DescribeInstanceStatus
          - ec2:DescribeInternetGateways
          - ec2:DescribeNetworkInterfaces
          - ec2:DescribeSecurityGroups
          - ec2:DescribeSubnets
          - ec2:DescribeTags
          - ec2:DescribeVpcs
          - ec2:ModifyInstanceAttribute
          - ec2:ModifyNetworkInterfaceAttribute
          - ec2:RevokeSecurityGroupIngress
          Resource: "*"
        - Effect: Allow
          Action:
          - elasticloadbalancing:AddListenerCertificates
          - elasticloadbalancing:AddTags
          - elasticloadbalancing:CreateListener
          - elasticloadbalancing:CreateLoadBalancer
          - elasticloadbalancing:CreateRule
          - elasticloadbalancing:CreateTargetGroup
          - elasticloadbalancing:DeleteListener
          - elasticloadbalancing:DeleteLoadBalancer
          - elasticloadbalancing:DeleteRule
          - elasticloadbalancing:DeleteTargetGroup
          - elasticloadbalancing:DeregisterTargets
          - elasticloadbalancing:DescribeListenerCertificates
          - elasticloadbalancing:DescribeListeners
          - elasticloadbalancing:DescribeLoadBalancers
          - elasticloadbalancing:DescribeLoadBalancerAttributes
          - elasticloadbalancing:DescribeRules
          - elasticloadbalancing:DescribeSSLPolicies
          - elasticloadbalancing:DescribeTags
          - elasticloadbalancing:DescribeTargetGroups
          - elasticloadbalancing:DescribeTargetGroupAttributes
          - elasticloadbalancing:DescribeTargetHealth
          - elasticloadbalancing:ModifyListener
          - elasticloadbalancing:ModifyLoadBalancerAttributes
          - elasticloadbalancing:ModifyRule
          - elasticloadbalancing:ModifyTargetGroup
          - elasticloadbalancing:ModifyTargetGroupAttributes
          - elasticloadbalancing:RegisterTargets
          - elasticloadbalancing:RemoveListenerCertificates
          - elasticloadbalancing:RemoveTags
          - elasticloadbalancing:SetIpAddressType
          - elasticloadbalancing:SetSecurityGroups
          - elasticloadbalancing:SetSubnets
          - elasticloadbalancing:SetWebACL
          Resource: "*"
        - Effect: Allow
          Action:
          - iam:CreateServiceLinkedRole
          - iam:GetServerCertificate
          - iam:ListServerCertificates
          Resource: "*"
        - Effect: Allow
          Action:
          - cognito-idp:DescribeUserPoolClient
          Resource: "*"
        - Effect: Allow
          Action:
          - waf-regional:GetWebACLForResource
          - waf-regional:GetWebACL
          - waf-regional:AssociateWebACL
          - waf-regional:DisassociateWebACL
          Resource: "*"
        - Effect: Allow
          Action:
          - tag:GetResources
          - tag:TagResources
          Resource: "*"
        - Effect: Allow
          Action:
          - waf:GetWebACL
          Resource: "*"
      Roles:
      - !Ref ManagedNodeGroupRoleName

`

// https://docs.aws.amazon.com/eks/latest/userguide/alb-ingress.html
// https://github.com/kubernetes-sigs/aws-alb-ingress-controller/blob/master/docs/examples/iam-policy.json
func (ts *tester) createALBPolicy() error {
	if ts.cfg.EKSConfig.AddOnALB2048.PolicyName == "" {
		return errors.New("empty AddOnALB2048.PolicyName")
	}
	if ts.cfg.EKSConfig.AddOnManagedNodeGroups.RoleName == "" {
		return errors.New("empty AddOnManagedNodeGroups.RoleName")
	}
	if ts.cfg.EKSConfig.AddOnALB2048.PolicyCFNStackID != "" {
		ts.cfg.Logger.Info("non-empty 2048 policy given; no need to create a new one")
		return nil
	}

	ts.cfg.Logger.Info("creating ALB Ingress Controller Policy", zap.String("policy-name", ts.cfg.EKSConfig.AddOnALB2048.PolicyName))
	stackInput := &cloudformation.CreateStackInput{
		StackName:    aws.String(ts.cfg.EKSConfig.AddOnALB2048.PolicyName),
		Capabilities: aws.StringSlice([]string{"CAPABILITY_NAMED_IAM"}),
		OnFailure:    aws.String(cloudformation.OnFailureDelete),
		TemplateBody: aws.String(TemplateALBIngressControllerPolicy),
		Tags: awscfn.NewTags(map[string]string{
			"Kind": "aws-k8s-tester",
			"Name": ts.cfg.EKSConfig.Name,
		}),
		Parameters: []*cloudformation.Parameter{
			{
				ParameterKey:   aws.String("ALBIngressControllerPolicyName"),
				ParameterValue: aws.String(ts.cfg.EKSConfig.AddOnALB2048.PolicyName),
			},
			{
				ParameterKey:   aws.String("ManagedNodeGroupRoleName"),
				ParameterValue: aws.String(ts.cfg.EKSConfig.AddOnManagedNodeGroups.RoleName),
			},
		},
	}
	stackOutput, err := ts.cfg.CloudFormationAPI.CreateStack(stackInput)
	if err != nil {
		return err
	}
	ts.cfg.EKSConfig.AddOnALB2048.PolicyCFNStackID = aws.StringValue(stackOutput.StackId)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	ch := awscfn.Poll(
		ctx,
		ts.cfg.Stopc,
		ts.cfg.Sig,
		ts.cfg.Logger,
		ts.cfg.CloudFormationAPI,
		ts.cfg.EKSConfig.AddOnALB2048.PolicyCFNStackID,
		cloudformation.ResourceStatusCreateComplete,
		25*time.Second,
		10*time.Second,
	)
	var st awscfn.StackStatus
	for st = range ch {
		if st.Error != nil {
			cancel()
			ts.cfg.EKSConfig.Status.ClusterStatus = fmt.Sprintf("failed to wait for ALB Ingress Controller Policy creation (%v)", st.Error)
			ts.cfg.EKSConfig.Sync()
			ts.cfg.Logger.Error("polling errror", zap.Error(st.Error))
		}
	}
	cancel()
	if st.Error != nil {
		return st.Error
	}

	ts.cfg.Logger.Info("created ALB Ingress Controller Policy",
		zap.String("policy-cfn-stack-id", ts.cfg.EKSConfig.AddOnALB2048.PolicyCFNStackID),
		zap.String("policy-name", ts.cfg.EKSConfig.AddOnALB2048.PolicyName),
	)

	return ts.cfg.EKSConfig.Sync()
}

// https://docs.aws.amazon.com/eks/latest/userguide/alb-ingress.html
// https://github.com/kubernetes-sigs/aws-alb-ingress-controller/blob/master/docs/examples/iam-policy.json
func (ts *tester) deleteALBPolicy() error {
	if ts.cfg.EKSConfig.AddOnALB2048.PolicyCFNStackID == "" {
		ts.cfg.Logger.Info("empty policy CFN stack ID; no need to delete policy")
		return nil
	}

	ts.cfg.Logger.Info("deleting ALB Ingress Controller Policy",
		zap.String("policy-cfn-stack-id", ts.cfg.EKSConfig.AddOnALB2048.PolicyCFNStackID),
	)
	_, err := ts.cfg.CloudFormationAPI.DeleteStack(&cloudformation.DeleteStackInput{
		StackName: aws.String(ts.cfg.EKSConfig.AddOnALB2048.PolicyCFNStackID),
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	ch := awscfn.Poll(
		ctx,
		make(chan struct{}),  // do not exit on stop
		make(chan os.Signal), // do not exit on stop
		ts.cfg.Logger,
		ts.cfg.CloudFormationAPI,
		ts.cfg.EKSConfig.AddOnALB2048.PolicyCFNStackID,
		cloudformation.ResourceStatusDeleteComplete,
		25*time.Second,
		10*time.Second,
	)
	var st awscfn.StackStatus
	for st = range ch {
		if st.Error != nil {
			cancel()
			ts.cfg.EKSConfig.Status.ClusterStatus = fmt.Sprintf("failed to wait for ALB Ingress Controller Policy deletion (%v)", st.Error)
			ts.cfg.EKSConfig.Sync()
			ts.cfg.Logger.Error("polling errror", zap.Error(st.Error))
		}
	}
	cancel()
	if st.Error != nil {
		return st.Error
	}
	ts.cfg.Logger.Info("deleted ALB Ingress Controller Policy",
		zap.String("policy-cfn-stack-id", ts.cfg.EKSConfig.AddOnALB2048.PolicyCFNStackID),
		zap.String("policy-name", ts.cfg.EKSConfig.AddOnALB2048.PolicyName),
	)

	return ts.cfg.EKSConfig.Sync()
}

// https://docs.aws.amazon.com/eks/latest/userguide/alb-ingress.html
// https://github.com/kubernetes-sigs/aws-alb-ingress-controller/blob/master/docs/examples/rbac-role.yaml
func (ts *tester) createALBServiceAccount() error {
	ts.cfg.Logger.Info("creating ALB Ingress Controller  ServiceAccount")
	_, err := ts.cfg.K8SClient.KubernetesClientSet().
		CoreV1().
		ServiceAccounts(albIngressControllerServiceAccountNamespace).
		Create(&v1.ServiceAccount{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "ServiceAccount",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      albIngressControllerServiceAccountName,
				Namespace: albIngressControllerServiceAccountNamespace,
				Labels: map[string]string{
					"app.kubernetes.io/name": albIngressControllerName,
				},
			},
		})
	if err != nil {
		return fmt.Errorf("failed to create ALB Ingress Controller ServiceAccount (%v)", err)
	}

	ts.cfg.Logger.Info("created ALB Ingress Controller ServiceAccount")
	return ts.cfg.EKSConfig.Sync()
}

// https://docs.aws.amazon.com/eks/latest/userguide/alb-ingress.html
// https://github.com/kubernetes-sigs/aws-alb-ingress-controller/blob/master/docs/examples/rbac-role.yaml
func (ts *tester) deleteALBServiceAccount() error {
	ts.cfg.Logger.Info("deleting ALB Ingress Controller ServiceAccount")
	foreground := metav1.DeletePropagationForeground
	err := ts.cfg.K8SClient.KubernetesClientSet().
		CoreV1().
		ServiceAccounts(albIngressControllerServiceAccountNamespace).
		Delete(
			albIngressControllerServiceAccountName,
			&metav1.DeleteOptions{
				GracePeriodSeconds: aws.Int64(0),
				PropagationPolicy:  &foreground,
			},
		)
	if err != nil && !strings.Contains(err.Error(), " not found") {
		return fmt.Errorf("failed to delete ALB Ingress Controller ServiceAccount (%v)", err)
	}
	ts.cfg.Logger.Info("deleted ALB Ingress Controller ServiceAccount", zap.Error(err))

	return ts.cfg.EKSConfig.Sync()
}

// https://docs.aws.amazon.com/eks/latest/userguide/alb-ingress.html
// https://github.com/kubernetes-sigs/aws-alb-ingress-controller/blob/master/docs/examples/rbac-role.yaml
func (ts *tester) createALBRBACClusterRole() error {
	ts.cfg.Logger.Info("creating ALB Ingress Controller RBAC ClusterRole")
	_, err := ts.cfg.K8SClient.KubernetesClientSet().
		RbacV1().
		ClusterRoles().
		Create(&rbacv1.ClusterRole{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "rbac.authorization.k8s.io/v1",
				Kind:       "ClusterRole",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      albIngressControllerRBACClusterRoleName,
				Namespace: albIngressControllerRBACClusterRoleNamespace,
				Labels: map[string]string{
					"app.kubernetes.io/name": albIngressControllerName,
				},
			},
			Rules: []rbacv1.PolicyRule{
				{ // read/write
					APIGroups: []string{
						"",
						"extensions",
					},
					Resources: []string{
						"configmaps",
						"endpoints",
						"events",
						"ingresses",
						"ingresses/status",
						"services",
					},
					Verbs: []string{
						"create",
						"get",
						"list",
						"update",
						"watch",
						"patch",
					},
				},
				{ // read-only
					APIGroups: []string{
						"",
						"extensions",
					},
					Resources: []string{
						"nodes",
						"pods",
						"secrets",
						"services",
						"namespaces",
					},
					Verbs: []string{
						"get",
						"list",
						"watch",
					},
				},
			},
		})
	if err != nil {
		return fmt.Errorf("failed to create ALB Ingress Controller RBAC ClusterRole (%v)", err)
	}

	ts.cfg.Logger.Info("created ALB Ingress Controller RBAC ClusterRole")
	return ts.cfg.EKSConfig.Sync()
}

// https://docs.aws.amazon.com/eks/latest/userguide/alb-ingress.html
// https://github.com/kubernetes-sigs/aws-alb-ingress-controller/blob/master/docs/examples/rbac-role.yaml
func (ts *tester) deleteALBRBACClusterRole() error {
	ts.cfg.Logger.Info("deleting ALB Ingress Controller RBAC ClusterRole")
	foreground := metav1.DeletePropagationForeground
	err := ts.cfg.K8SClient.KubernetesClientSet().
		RbacV1().
		ClusterRoles().
		Delete(
			albIngressControllerRBACClusterRoleName,
			&metav1.DeleteOptions{
				GracePeriodSeconds: aws.Int64(0),
				PropagationPolicy:  &foreground,
			},
		)
	if err != nil && !strings.Contains(err.Error(), " not found") {
		return fmt.Errorf("failed to delete ALB Ingress Controller RBAC ClusterRole (%v)", err)
	}

	ts.cfg.Logger.Info("deleted ALB Ingress Controller RBAC ClusterRole", zap.Error(err))
	return ts.cfg.EKSConfig.Sync()
}

// https://docs.aws.amazon.com/eks/latest/userguide/alb-ingress.html
// https://github.com/kubernetes-sigs/aws-alb-ingress-controller/blob/master/docs/examples/rbac-role.yaml
func (ts *tester) createALBRBACClusterRoleBinding() error {
	ts.cfg.Logger.Info("creating ALB Ingress Controller RBAC ClusterRoleBinding")
	_, err := ts.cfg.K8SClient.KubernetesClientSet().
		RbacV1().
		ClusterRoleBindings().
		Create(&rbacv1.ClusterRoleBinding{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "rbac.authorization.k8s.io/v1",
				Kind:       "ClusterRoleBinding",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      albIngressControllerRBACClusterRoleBindingName,
				Namespace: albIngressControllerRBACClusterRoleBindingNamespace,
				Labels: map[string]string{
					"app.kubernetes.io/name": albIngressControllerName,
				},
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      "ServiceAccount",
					Name:      albIngressControllerServiceAccountName,
					Namespace: albIngressControllerServiceAccountNamespace,
				},
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "ClusterRole",
				Name:     albIngressControllerRBACClusterRoleName,
			},
		})
	if err != nil {
		return fmt.Errorf("failed to create ALB Ingress Controller RBAC ClusterRoleBinding (%v)", err)
	}

	ts.cfg.Logger.Info("created ALB Ingress Controller RBAC ClusterRoleBinding")
	return ts.cfg.EKSConfig.Sync()
}

// https://docs.aws.amazon.com/eks/latest/userguide/alb-ingress.html
// https://github.com/kubernetes-sigs/aws-alb-ingress-controller/blob/master/docs/examples/rbac-role.yaml
func (ts *tester) deleteALBRBACClusterRoleBinding() error {
	ts.cfg.Logger.Info("deleting ALB Ingress Controller RBAC ClusterRoleBinding")
	foreground := metav1.DeletePropagationForeground
	err := ts.cfg.K8SClient.KubernetesClientSet().
		RbacV1().
		ClusterRoleBindings().
		Delete(
			albIngressControllerRBACClusterRoleBindingName,
			&metav1.DeleteOptions{
				GracePeriodSeconds: aws.Int64(0),
				PropagationPolicy:  &foreground,
			},
		)
	if err != nil && !strings.Contains(err.Error(), " not found") {
		return fmt.Errorf("failed to delete ALB Ingress Controller RBAC ClusterRoleBinding (%v)", err)
	}

	ts.cfg.Logger.Info("deleted ALB Ingress Controller RBAC ClusterRoleBinding", zap.Error(err))
	return ts.cfg.EKSConfig.Sync()
}

// https://docs.aws.amazon.com/eks/latest/userguide/alb-ingress.html
// https://github.com/kubernetes-sigs/aws-alb-ingress-controller/blob/master/docs/examples/alb-ingress-controller.yaml
func (ts *tester) createALBDeployment() error {
	ts.cfg.Logger.Info("creating ALB Ingress Controller Deployment")
	_, err := ts.cfg.K8SClient.KubernetesClientSet().
		AppsV1().
		Deployments(albIngressControllerDeploymentNamespace).
		Create(&appsv1.Deployment{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      albIngressControllerDeploymentName,
				Namespace: albIngressControllerDeploymentNamespace,
				Labels: map[string]string{
					"app.kubernetes.io/name": albIngressControllerName,
				},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: aws.Int32(ts.cfg.EKSConfig.AddOnALB2048.DeploymentReplicasALB),
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app.kubernetes.io/name": albIngressControllerName,
					},
				},
				Template: v1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app.kubernetes.io/name": albIngressControllerName,
						},
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name:            albIngressControllerDeploymentName,
								Image:           ALBImageName,
								ImagePullPolicy: v1.PullAlways,
								Args: []string{
									"--ingress-class=alb",
									fmt.Sprintf("--cluster-name=%s", ts.cfg.EKSConfig.Name),
									fmt.Sprintf("--aws-vpc-id=%s", ts.cfg.EKSConfig.Status.VPCID),
									fmt.Sprintf("--aws-region=%s", ts.cfg.EKSConfig.Region),
								},
							},
						},
						ServiceAccountName: albIngressControllerServiceAccountName,
					},
				},
			},
		})
	if err != nil {
		return fmt.Errorf("failed to create ALB Ingress Controller Deployment (%v)", err)
	}

	ts.cfg.Logger.Info("created ALB Ingress Controller Deployment")
	return ts.cfg.EKSConfig.Sync()
}

// https://docs.aws.amazon.com/eks/latest/userguide/alb-ingress.html
// https://github.com/kubernetes-sigs/aws-alb-ingress-controller/blob/master/docs/examples/alb-ingress-controller.yaml
func (ts *tester) deleteALBDeployment() error {
	ts.cfg.Logger.Info("deleting ALB Ingress Controller Deployment")
	foreground := metav1.DeletePropagationForeground
	err := ts.cfg.K8SClient.KubernetesClientSet().
		AppsV1().
		Deployments(albIngressControllerDeploymentNamespace).
		Delete(
			albIngressControllerDeploymentName,
			&metav1.DeleteOptions{
				GracePeriodSeconds: aws.Int64(0),
				PropagationPolicy:  &foreground,
			},
		)
	if err != nil && !strings.Contains(err.Error(), " not found") {
		return fmt.Errorf("failed to delete ALB Ingress Controller Deployment (%v)", err)
	}

	ts.cfg.Logger.Info("deleted ALB Ingress Controller Deployment", zap.Error(err))
	return ts.cfg.EKSConfig.Sync()
}

func (ts *tester) waitDeploymentALB() error {
	ts.cfg.Logger.Info("waiting for ALB Deployment")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	output, err := exec.New().CommandContext(
		ctx,
		ts.cfg.EKSConfig.KubectlPath,
		"--kubeconfig="+ts.cfg.EKSConfig.KubeConfigPath,
		"--namespace="+albIngressControllerDeploymentNamespace,
		"describe",
		"deployment",
		albIngressControllerDeploymentName,
	).CombinedOutput()
	cancel()
	if err != nil {
		return fmt.Errorf("'kubectl describe deployment' failed %v", err)
	}
	out := string(output)
	colorstring.Printf("\n\n\"[light_green]kubectl describe deployment[default]\" output:\n%s\n\n", out)

	ready := false
	waitDur := 5*time.Minute + time.Duration(ts.cfg.EKSConfig.AddOnALB2048.DeploymentReplicasALB)*time.Minute
	retryStart := time.Now()
	for time.Now().Sub(retryStart) < waitDur {
		select {
		case <-ts.cfg.Stopc:
			return errors.New("check aborted")
		case <-ts.cfg.Sig:
			return errors.New("check aborted")
		case <-time.After(15 * time.Second):
		}

		dresp, err := ts.cfg.K8SClient.KubernetesClientSet().
			AppsV1().
			Deployments(albIngressControllerDeploymentNamespace).
			Get(albIngressControllerDeploymentName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get Deployment (%v)", err)
		}
		ts.cfg.Logger.Info("get deployment",
			zap.Int32("desired-replicas", dresp.Status.Replicas),
			zap.Int32("available-replicas", dresp.Status.AvailableReplicas),
			zap.Int32("unavailable-replicas", dresp.Status.UnavailableReplicas),
			zap.Int32("ready-replicas", dresp.Status.ReadyReplicas),
		)
		available := false
		for _, cond := range dresp.Status.Conditions {
			ts.cfg.Logger.Info("condition",
				zap.String("last-updated", cond.LastUpdateTime.String()),
				zap.String("type", string(cond.Type)),
				zap.String("status", string(cond.Status)),
				zap.String("reason", cond.Reason),
				zap.String("message", cond.Message),
			)
			if cond.Status != v1.ConditionTrue {
				continue
			}
			if cond.Type == appsv1.DeploymentAvailable {
				available = true
				break
			}
		}
		if available && dresp.Status.AvailableReplicas >= ts.cfg.EKSConfig.AddOnALB2048.DeploymentReplicasALB {
			ready = true
			break
		}
	}
	if !ready {
		return errors.New("Deployment not ready")
	}

	ts.cfg.Logger.Info("waited for ALB Deployment")
	return ts.cfg.EKSConfig.Sync()
}

// https://docs.aws.amazon.com/eks/latest/userguide/alb-ingress.html
// https://github.com/kubernetes-sigs/aws-alb-ingress-controller/blob/master/docs/examples/2048/2048-deployment.yaml
func (ts *tester) create2048Deployment() error {
	ts.cfg.Logger.Info("creating ALB 2048 Deployment")
	_, err := ts.cfg.K8SClient.KubernetesClientSet().
		AppsV1().
		Deployments(ts.cfg.EKSConfig.AddOnALB2048.Namespace).
		Create(&appsv1.Deployment{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      alb2048DeploymentName,
				Namespace: ts.cfg.EKSConfig.AddOnALB2048.Namespace,
				Labels: map[string]string{
					"app": alb2048AppName,
				},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: aws.Int32(ts.cfg.EKSConfig.AddOnALB2048.DeploymentReplicas2048),
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": alb2048AppName,
					},
				},
				Template: v1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app": alb2048AppName,
						},
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name:            alb2048AppName,
								Image:           alb2048ImageName,
								ImagePullPolicy: v1.PullAlways,
								Ports: []v1.ContainerPort{
									{
										Protocol:      v1.ProtocolTCP,
										ContainerPort: 80,
									},
								},
							},
						},
					},
				},
			},
		})
	if err != nil {
		return fmt.Errorf("failed to create ALB 2048 Deployment (%v)", err)
	}
	ts.cfg.Logger.Info("created ALB 2048 Deployment")

	return ts.cfg.EKSConfig.Sync()
}

// https://docs.aws.amazon.com/eks/latest/userguide/alb-ingress.html
// https://github.com/kubernetes-sigs/aws-alb-ingress-controller/blob/master/docs/examples/2048/2048-deployment.yaml
func (ts *tester) delete2048Deployment() error {
	ts.cfg.Logger.Info("deleting ALB 2048 Deployment")
	foreground := metav1.DeletePropagationForeground
	err := ts.cfg.K8SClient.KubernetesClientSet().
		AppsV1().
		Deployments(ts.cfg.EKSConfig.AddOnALB2048.Namespace).
		Delete(
			alb2048DeploymentName,
			&metav1.DeleteOptions{
				GracePeriodSeconds: aws.Int64(0),
				PropagationPolicy:  &foreground,
			},
		)
	if err != nil && !strings.Contains(err.Error(), " not found") {
		return fmt.Errorf("failed to delete ALB 2048 Deployment (%v)", err)
	}

	ts.cfg.Logger.Info("deleted ALB 2048 Deployment", zap.Error(err))
	return ts.cfg.EKSConfig.Sync()
}

func (ts *tester) waitDeployment2048() error {
	ts.cfg.Logger.Info("waiting for 2048 Deployment")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	output, err := exec.New().CommandContext(
		ctx,
		ts.cfg.EKSConfig.KubectlPath,
		"--kubeconfig="+ts.cfg.EKSConfig.KubeConfigPath,
		"--namespace="+ts.cfg.EKSConfig.AddOnALB2048.Namespace,
		"describe",
		"deployment",
		alb2048DeploymentName,
	).CombinedOutput()
	cancel()
	if err != nil {
		return fmt.Errorf("'kubectl describe deployment' failed %v", err)
	}
	out := string(output)
	colorstring.Printf("\n\n\"[light_green]kubectl describe deployment[default]\" output:\n%s\n\n", out)

	ready := false
	waitDur := 5*time.Minute + time.Duration(ts.cfg.EKSConfig.AddOnALB2048.DeploymentReplicas2048)*time.Minute
	retryStart := time.Now()
	for time.Now().Sub(retryStart) < waitDur {
		select {
		case <-ts.cfg.Stopc:
			return errors.New("check aborted")
		case <-ts.cfg.Sig:
			return errors.New("check aborted")
		case <-time.After(15 * time.Second):
		}

		dresp, err := ts.cfg.K8SClient.KubernetesClientSet().
			AppsV1().
			Deployments(ts.cfg.EKSConfig.AddOnALB2048.Namespace).
			Get(alb2048DeploymentName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get Deployment (%v)", err)
		}
		ts.cfg.Logger.Info("get deployment",
			zap.Int32("desired-replicas", dresp.Status.Replicas),
			zap.Int32("available-replicas", dresp.Status.AvailableReplicas),
			zap.Int32("unavailable-replicas", dresp.Status.UnavailableReplicas),
			zap.Int32("ready-replicas", dresp.Status.ReadyReplicas),
		)
		available := false
		for _, cond := range dresp.Status.Conditions {
			ts.cfg.Logger.Info("condition",
				zap.String("last-updated", cond.LastUpdateTime.String()),
				zap.String("type", string(cond.Type)),
				zap.String("status", string(cond.Status)),
				zap.String("reason", cond.Reason),
				zap.String("message", cond.Message),
			)
			if cond.Status != v1.ConditionTrue {
				continue
			}
			if cond.Type == appsv1.DeploymentAvailable {
				available = true
				break
			}
		}
		if available && dresp.Status.AvailableReplicas >= ts.cfg.EKSConfig.AddOnALB2048.DeploymentReplicas2048 {
			ready = true
			break
		}
	}
	if !ready {
		return errors.New("Deployment not ready")
	}

	ts.cfg.Logger.Info("waited for 2048 Deployment")
	return ts.cfg.EKSConfig.Sync()
}

// https://docs.aws.amazon.com/eks/latest/userguide/alb-ingress.html
// https://github.com/kubernetes-sigs/aws-alb-ingress-controller/blob/master/docs/examples/2048/2048-service.yaml
func (ts *tester) create2048Service() error {
	ts.cfg.Logger.Info("creating ALB 2048 Service")
	_, err := ts.cfg.K8SClient.KubernetesClientSet().
		CoreV1().
		Services(ts.cfg.EKSConfig.AddOnALB2048.Namespace).
		Create(&v1.Service{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "Service",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      alb2048ServiceName,
				Namespace: ts.cfg.EKSConfig.AddOnALB2048.Namespace,
			},
			Spec: v1.ServiceSpec{
				Selector: map[string]string{
					"app": alb2048AppName,
				},
				Type: v1.ServiceTypeNodePort,
				Ports: []v1.ServicePort{
					{
						Protocol:   v1.ProtocolTCP,
						Port:       80,
						TargetPort: intstr.FromInt(80),
					},
				},
			},
		})
	if err != nil {
		return fmt.Errorf("failed to create ALB 2048 Service (%v)", err)
	}

	ts.cfg.Logger.Info("created ALB 2048 Service")
	return ts.cfg.EKSConfig.Sync()
}

// https://docs.aws.amazon.com/eks/latest/userguide/alb-ingress.html
// https://github.com/kubernetes-sigs/aws-alb-ingress-controller/blob/master/docs/examples/2048/2048-service.yaml
func (ts *tester) delete2048Service() error {
	ts.cfg.Logger.Info("deleting ALB 2048 Service")
	foreground := metav1.DeletePropagationForeground
	err := ts.cfg.K8SClient.KubernetesClientSet().
		CoreV1().
		Services(ts.cfg.EKSConfig.AddOnALB2048.Namespace).
		Delete(
			alb2048ServiceName,
			&metav1.DeleteOptions{
				GracePeriodSeconds: aws.Int64(0),
				PropagationPolicy:  &foreground,
			},
		)
	if err != nil && !strings.Contains(err.Error(), " not found") {
		return fmt.Errorf("failed to delete ALB 2048 Service (%v)", err)
	}

	ts.cfg.Logger.Info("deleted ALB 2048 Service", zap.Error(err))
	return ts.cfg.EKSConfig.Sync()
}

// https://docs.aws.amazon.com/eks/latest/userguide/alb-ingress.html
// https://github.com/kubernetes-sigs/aws-alb-ingress-controller/blob/master/docs/examples/2048/2048-ingress.yaml
func (ts *tester) create2048Ingress() error {
	ts.cfg.Logger.Info("creating ALB 2048 Ingress")
	_, err := ts.cfg.K8SClient.KubernetesClientSet().
		ExtensionsV1beta1().
		Ingresses(ts.cfg.EKSConfig.AddOnALB2048.Namespace).
		Create(&v1beta1.Ingress{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "extensions/v1beta1",
				Kind:       "Ingress",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      alb2048IngressName,
				Namespace: ts.cfg.EKSConfig.AddOnALB2048.Namespace,
				Annotations: map[string]string{
					"kubernetes.io/ingress.class":      "alb",
					"alb.ingress.kubernetes.io/scheme": "internet-facing",
				},
				Labels: map[string]string{
					"app": alb2048AppName,
				},
			},
			Spec: v1beta1.IngressSpec{
				Rules: []v1beta1.IngressRule{
					{
						IngressRuleValue: v1beta1.IngressRuleValue{
							HTTP: &v1beta1.HTTPIngressRuleValue{
								Paths: []v1beta1.HTTPIngressPath{
									{
										Path: "/*",
										Backend: v1beta1.IngressBackend{
											ServiceName: alb2048ServiceName,
											ServicePort: intstr.FromInt(80),
										},
									},
								},
							},
						},
					},
				},
			},
		})
	if err != nil {
		return fmt.Errorf("failed to create ALB 2048 Ingress (%v)", err)
	}
	ts.cfg.Logger.Info("created ALB 2048 Ingress")

	waitDur := 3 * time.Minute
	ts.cfg.Logger.Info("waiting for ALB 2048 Ingress", zap.Duration("wait", waitDur))
	select {
	case <-ts.cfg.Stopc:
		return errors.New("ALB 2048 Ingress creation aborted")
	case sig := <-ts.cfg.Sig:
		return fmt.Errorf("received os signal %v", sig)
	case <-time.After(waitDur):
	}

	hostName := ""
	retryStart := time.Now()
	for time.Now().Sub(retryStart) < waitDur {
		select {
		case <-ts.cfg.Stopc:
			return errors.New("ALB 2048 Ingress creation aborted")
		case sig := <-ts.cfg.Sig:
			return fmt.Errorf("received os signal %v", sig)
		case <-time.After(5 * time.Second):
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		clusterInfoOut, err := exec.New().CommandContext(
			ctx,
			ts.cfg.EKSConfig.KubectlPath,
			"--kubeconfig="+ts.cfg.EKSConfig.KubeConfigPath,
			"--namespace="+ts.cfg.EKSConfig.AddOnALB2048.Namespace,
			"describe",
			"svc",
			alb2048ServiceName,
		).CombinedOutput()
		cancel()
		if err != nil {
			ts.cfg.Logger.Warn("'kubectl describe svc' failed", zap.Error(err))
		} else {
			out := string(clusterInfoOut)
			colorstring.Printf("\n\n\"[light_green]kubectl describe svc %s[default]\" output:\n%s\n\n", alb2048ServiceName, out)
		}

		ts.cfg.Logger.Info("querying ALB 2048 Ingress for HTTP endpoint")
		so, err := ts.cfg.K8SClient.KubernetesClientSet().
			ExtensionsV1beta1().
			Ingresses(ts.cfg.EKSConfig.AddOnALB2048.Namespace).
			Get(alb2048IngressName, metav1.GetOptions{})
		if err != nil {
			ts.cfg.Logger.Error("failed to get ALB 2048 Ingress; retrying", zap.Error(err))
			time.Sleep(5 * time.Second)
			continue
		}

		ts.cfg.Logger.Info(
			"ALB 2048 Ingress has been linked to LoadBalancer",
			zap.String("load-balancer", fmt.Sprintf("%+v", so.Status.LoadBalancer)),
		)
		for _, ing := range so.Status.LoadBalancer.Ingress {
			ts.cfg.Logger.Info(
				"ALB 2048 Ingress has been linked to LoadBalancer.Ingress",
				zap.String("ingress", fmt.Sprintf("%+v", ing)),
			)
			hostName = ing.Hostname
			break
		}
		if hostName != "" {
			ts.cfg.Logger.Info("found ALB Ingress host name", zap.String("host-name", hostName))
			break
		}
	}
	if hostName == "" {
		return errors.New("failed to find ALB host name")
	}

	fields := strings.Split(hostName, "-")
	if len(fields) >= 3 {
		ts.cfg.EKSConfig.AddOnALB2048.ALBName = strings.Join(fields[:3], "-")
	}
	ts.cfg.EKSConfig.AddOnALB2048.URL = "http://" + hostName
	ts.cfg.EKSConfig.Sync()

	do, err := ts.cfg.ELB2API.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{
		Names: aws.StringSlice([]string{ts.cfg.EKSConfig.AddOnALB2048.ALBName}),
	})
	if err != nil {
		return err
	}
	for _, lb := range do.LoadBalancers {
		ts.cfg.EKSConfig.AddOnALB2048.ALBARN = aws.StringValue(lb.LoadBalancerArn)
		break
	}

	colorstring.Printf("\n[light_green]ALB 2048 ARN: [default]%s\n", ts.cfg.EKSConfig.AddOnALB2048.ALBARN)
	colorstring.Printf("[light_green]ALB 2048 Name: [default]%s\n", ts.cfg.EKSConfig.AddOnALB2048.ALBName)
	colorstring.Printf("[light_green]ALB 2048 URL:[default]\n%s\n\n", ts.cfg.EKSConfig.AddOnALB2048.URL)

	ts.cfg.Logger.Info("waiting before testing ALB 2048 Ingress")
	time.Sleep(10 * time.Second)

	retryStart = time.Now()
	for time.Now().Sub(retryStart) < waitDur {
		select {
		case <-ts.cfg.Stopc:
			return errors.New("ALB 2048 Ingress creation aborted")
		case sig := <-ts.cfg.Sig:
			return fmt.Errorf("received os signal %v", sig)
		case <-time.After(5 * time.Second):
		}

		buf := bytes.NewBuffer(nil)
		err = httpReadInsecure(ts.cfg.Logger, ts.cfg.EKSConfig.AddOnALB2048.URL, buf)
		if err != nil {
			ts.cfg.Logger.Error("failed to read ALB 2048 Service; retrying", zap.Error(err))
			time.Sleep(5 * time.Second)
			continue
		}

		httpOutput := buf.String()
		fmt.Printf("\nALB 2048 Ingress output:\n%s\n", httpOutput)

		if strings.Contains(httpOutput, `2048 tile!`) {
			ts.cfg.Logger.Info(
				"read ALB 2048 Service; exiting",
				zap.String("host-name", hostName),
			)
			break
		}

		ts.cfg.Logger.Warn("unexpected ALB 2048 Ingress output; retrying")
	}

	return ts.cfg.EKSConfig.Sync()
}

// https://docs.aws.amazon.com/eks/latest/userguide/alb-ingress.html
// https://github.com/kubernetes-sigs/aws-alb-ingress-controller/blob/master/docs/examples/2048/2048-ingress.yaml
func (ts *tester) delete2048Ingress() error {
	ts.cfg.Logger.Info("deleting ALB 2048 Ingress")
	foreground := metav1.DeletePropagationForeground
	err := ts.cfg.K8SClient.KubernetesClientSet().
		ExtensionsV1beta1().
		Ingresses(ts.cfg.EKSConfig.AddOnALB2048.Namespace).
		Delete(
			alb2048IngressName,
			&metav1.DeleteOptions{
				GracePeriodSeconds: aws.Int64(0),
				PropagationPolicy:  &foreground,
			},
		)
	if err != nil && !strings.Contains(err.Error(), " not found") {
		return fmt.Errorf("failed to delete ALB 2048 Ingress (%v)", err)
	}
	ts.cfg.Logger.Info("deleted ALB 2048 Ingress", zap.Error(err))

	return ts.cfg.EKSConfig.Sync()
}

// curl -k [URL]
func httpReadInsecure(lg *zap.Logger, u string, wr io.Writer) error {
	lg.Info("reading", zap.String("url", u))
	cli := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		}}
	r, err := cli.Get(u)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode >= 400 {
		return fmt.Errorf("%q returned %d", u, r.StatusCode)
	}

	_, err = io.Copy(wr, r.Body)
	if err != nil {
		lg.Warn("failed to read", zap.String("url", u), zap.Error(err))
	} else {
		lg.Info("read", zap.String("url", u))
	}
	return err
}