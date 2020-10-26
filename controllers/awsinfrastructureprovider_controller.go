/*
Copyright 2020 Critical Stack, LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/criticalstack/machine-api/util"
	"github.com/go-logr/logr"
	"github.com/go-openapi/spec"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/criticalstack/machine-api-provider-aws/api/v1alpha1"
)

const (
	lastUpdatedAnnotation = "infrastructure.crit.sh/lastUpdated"
	secretAgeThreshold    = 5 * time.Minute
)

// AWSInfrastructureProviderReconciler reconciles a AWSInfrastructureProvider object
type AWSInfrastructureProviderReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme

	config *rest.Config
}

func (r *AWSInfrastructureProviderReconciler) SetupWithManager(mgr ctrl.Manager, options controller.Options) error {
	r.config = mgr.GetConfig()
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.AWSInfrastructureProvider{}).
		Owns(&v1.Secret{}).
		WithOptions(options).
		Complete(r)
}

// +kubebuilder:rbac:groups=infrastructure.crit.sh,resources=awsinfrastructureproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.crit.sh,resources=awsinfrastructureproviders/status,verbs=create;update
// +kubebuilder:rbac:groups=machine.crit.sh,resources=infrastructureproviders;infrastructureproviders/status,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=*

func (r *AWSInfrastructureProviderReconciler) Reconcile(req ctrl.Request) (_ ctrl.Result, reterr error) {
	ctx := context.Background()
	log := r.Log.WithValues("awsinfrastructureprovider", req.NamespacedName)

	ip := &v1alpha1.AWSInfrastructureProvider{}
	if err := r.Get(ctx, req.NamespacedName, ip); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	ipOwner, err := util.GetOwnerInfrastructureProvider(ctx, r.Client, ip.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to retrieve owner infraprovider")
	}
	if ipOwner == nil {
		log.Info("InfrastructureProvider Controller has not yet set OwnerRef")
		return ctrl.Result{}, nil
	}

	log = log.WithValues("infrastructureprovider", ipOwner.Name)

	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      OpenAPISchemaSecretName,
			Namespace: ip.Namespace,
		},
	}
	if err := r.Get(ctx, client.ObjectKey{Name: s.Name, Namespace: s.Namespace}, s); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}

	ip.Status.Ready = !s.GetCreationTimestamp().Time.IsZero() // ready if secret already exists
	if err := r.Status().Update(ctx, ip); err != nil {
		log.Error(err, "failed to update provider status")
		return ctrl.Result{}, err
	}

	if a := s.GetAnnotations(); a != nil {
		if t, _ := time.Parse(time.RFC3339, a[lastUpdatedAnnotation]); time.Since(t) < secretAgeThreshold {
			d := time.Until(t.Add(secretAgeThreshold))
			log.Info("update not needed", "delay", d)
			return ctrl.Result{RequeueAfter: d}, nil
		}
	}

	schema, err := r.schema(ctx, ip)
	if err != nil {
		return ctrl.Result{}, err
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return ctrl.Result{}, err
	}

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, s, func() error {
		s.Data = map[string][]byte{"schema": b}
		a := s.GetAnnotations()
		if a == nil {
			a = make(map[string]string)
		}
		a[lastUpdatedAnnotation] = time.Now().Format(time.RFC3339)
		s.SetAnnotations(a)
		return controllerutil.SetControllerReference(ip, s, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

const OpenAPISchemaSecretName = "config-schema"

func getName(tags []*ec2.Tag) string {
	var name string
	for _, t := range tags {
		if aws.StringValue(t.Key) == "Name" {
			name = aws.StringValue(t.Value)
			break
		}
	}
	if name == "" {
		name = "<no name>"
	}
	return name
}

func (r *AWSInfrastructureProviderReconciler) schema(ctx context.Context, ip *v1alpha1.AWSInfrastructureProvider) (*spec.Schema, error) {
	awscfg := &aws.Config{Region: aws.String(ip.Spec.Region)}
	svc := ec2.New(session.New(awscfg))
	iamSvc := iam.New(session.New(awscfg))

	describeInstanceTypesInput := &ec2.DescribeInstanceTypeOfferingsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("location"),
				Values: aws.StringSlice([]string{ip.Spec.Region}),
			},
		},
		MaxResults: aws.Int64(1000),
	}
	if ip.Spec.InstanceTypeFilter != "" {
		describeInstanceTypesInput.Filters = append(describeInstanceTypesInput.Filters, &ec2.Filter{
			Name:   aws.String("instance-type"),
			Values: aws.StringSlice([]string{ip.Spec.InstanceTypeFilter}),
		})
	}
	resp, err := svc.DescribeInstanceTypeOfferingsWithContext(ctx, describeInstanceTypesInput)
	if err != nil {
		return nil, err
	}
	instanceTypes := make([]interface{}, 0)
	for _, x := range resp.InstanceTypeOfferings {
		instanceTypes = append(instanceTypes, aws.StringValue(x.InstanceType))
	}

	describeImagesInput := &ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{
			{
				// TODO(ktravis): just make this a default imageFilter
				Name:   aws.String("architecture"),
				Values: aws.StringSlice([]string{"x86_64"}),
			},
		},
		Owners: aws.StringSlice([]string{"self"}),
	}
	for _, f := range ip.Spec.ImageFilters {
		describeImagesInput.Filters = append(describeImagesInput.Filters, &ec2.Filter{
			Name:   aws.String(f.Name),
			Values: aws.StringSlice(f.Values),
		})
	}
	imgResp, err := svc.DescribeImagesWithContext(ctx, describeImagesInput)
	if err != nil {
		return nil, err
	}
	images := make([]interface{}, 0)
	for _, x := range imgResp.Images {
		images = append(images, aws.StringValue(x.Name))
	}

	describeSecurityGroupsInput := &ec2.DescribeSecurityGroupsInput{}
	for _, f := range ip.Spec.SecurityGroupFilters {
		describeSecurityGroupsInput.Filters = append(describeSecurityGroupsInput.Filters, &ec2.Filter{
			Name:   aws.String(f.Name),
			Values: aws.StringSlice(f.Values),
		})
	}
	sgResp, err := svc.DescribeSecurityGroupsWithContext(ctx, describeSecurityGroupsInput)
	if err != nil {
		return nil, err
	}
	// TODO(ktravis): do the same as below with subnets, add security group rules as description
	securityGroups := make([]spec.Schema, 0)
	for _, x := range sgResp.SecurityGroups {
		securityGroups = append(securityGroups, *spec.StringProperty().
			WithTitle(fmt.Sprintf("%s (%s)", aws.StringValue(x.GroupName), aws.StringValue(x.GroupId))).
			WithEnum(aws.StringValue(x.GroupId)),
		)
	}

	describeSubnetsInput := &ec2.DescribeSubnetsInput{
		MaxResults: aws.Int64(100),
	}
	for _, f := range ip.Spec.SubnetFilters {
		describeSubnetsInput.Filters = append(describeSubnetsInput.Filters, &ec2.Filter{
			Name:   aws.String(f.Name),
			Values: aws.StringSlice(f.Values),
		})
	}
	subResp, err := svc.DescribeSubnetsWithContext(ctx, describeSubnetsInput)
	if err != nil {
		return nil, err
	}
	subnetOptions := make([]spec.Schema, 0)
	for _, x := range subResp.Subnets {
		subnetOptions = append(subnetOptions, *spec.StringProperty().
			WithTitle(fmt.Sprintf("%s (%s)", getName(x.Tags), aws.StringValue(x.SubnetId))).
			WithEnum(aws.StringValue(x.SubnetId)),
		)
	}

	listInstanceProfilesInput := &iam.ListInstanceProfilesInput{
		MaxItems: aws.Int64(1000),
	}
	ipResp, err := iamSvc.ListInstanceProfilesWithContext(ctx, listInstanceProfilesInput)
	if err != nil {
		return nil, err
	}
	instanceProfiles := make([]interface{}, 0)
	for _, x := range ipResp.InstanceProfiles {
		instanceProfiles = append(instanceProfiles, aws.StringValue(x.InstanceProfileName))
	}

	required := []spec.SchemaProps{
		{
			ID:          "instanceType",
			Title:       "Instance Type",
			Type:        spec.StringOrArray{"string"},
			Enum:        instanceTypes,
			Description: "type of instance",
			Default:     "",
		},

		{
			ID:          "machineImage",
			Title:       "Machine Image",
			Type:        spec.StringOrArray{"string"},
			Description: "AMI to use",
			Enum:        images,
			Default:     "",
		},

		{
			ID:          "blockDevices",
			Title:       "Block Devices",
			Type:        spec.StringOrArray{"array"},
			Description: "Block Devices",
			Default: []interface{}{
				map[string]interface{}{
					"deviceName": "/dev/sda1",
					"volumeSize": 100,
					"volumeType": "ebs",
					"encrypted":  true,
				},
			},
			Items: &spec.SchemaOrArray{
				Schema: &spec.Schema{
					SchemaProps: spec.SchemaProps{
						Type: spec.StringOrArray{"object"},
						Properties: map[string]spec.Schema{
							"deviceName": *spec.StringProperty(),
							"volumeSize": *spec.Int64Property(),
							"volumeType": *spec.StringProperty().WithDefault("ebs"),
							"encrypted":  *spec.BoolProperty().WithDefault(true),
						},
						Required: []string{"deviceName", "volumeSize", "volumeType"},
					},
				},
			},
		},

		spec.StringProperty().
			WithID("region").
			WithTitle("Region").
			WithDefault(ip.Spec.Region).SchemaProps,

		spec.ArrayProperty(
			&spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type:  spec.StringOrArray{"string"},
					AnyOf: subnetOptions,
				},
			},
		).
			WithID("subnetIDs").
			WithTitle("Subnets").SchemaProps,

		spec.StringProperty().
			WithID("iamInstanceProfile").
			WithTitle("IAM Instance Profile").
			WithEnum(instanceProfiles...).SchemaProps,

		spec.ArrayProperty(
			&spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type:  spec.StringOrArray{"string"},
					AnyOf: securityGroups,
				},
			},
		).
			WithID("securityGroupIDs").
			WithTitle("SecurityGroups").SchemaProps,
		// TODO(ktravis): more detail in the options, optional things - tags
		// etc ...
	}

	props := make(map[string]spec.Schema)
	requiredIDs := make([]string, 0)
	for _, p := range required {
		requiredIDs = append(requiredIDs, p.ID)
		props[p.ID] = spec.Schema{SchemaProps: p}
	}
	return &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type:  spec.StringOrArray{"object"},
			Title: "AWS Worker Config",
			Properties: map[string]spec.Schema{
				"apiVersion": {
					SchemaProps: spec.SchemaProps{
						Type:    spec.StringOrArray{"string"},
						Default: v1alpha1.GroupVersion.String(),
					},
				},
				"kind": {
					SchemaProps: spec.SchemaProps{
						Type:    spec.StringOrArray{"string"},
						Default: "AWSMachine",
					},
				},
				"metadata": {
					SchemaProps: spec.SchemaProps{
						Type:  spec.StringOrArray{"object"},
						Title: "Metadata",
						Properties: map[string]spec.Schema{
							"name": {
								SchemaProps: spec.SchemaProps{
									Type: spec.StringOrArray{"string"},
								},
							},
						},
						Required: []string{"name"},
					},
				},
				"spec": {
					SchemaProps: spec.SchemaProps{
						Type:       spec.StringOrArray{"object"},
						Properties: props,
						Required:   requiredIDs,
					},
				},
			},
		},
	}, nil
}
