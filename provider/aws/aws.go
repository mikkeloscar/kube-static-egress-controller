package aws

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/cloudformation/cloudformationiface"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	cft "github.com/crewjam/go-cloudformation"
	"github.com/linki/instrumented_http"
	log "github.com/sirupsen/logrus"
)

const (
	ProviderName                        = "aws"
	stackName                           = "egress-static-nat"
	parameterVPCIDParameter             = "VPCIDParameter"
	parameterInternetGatewayIDParameter = "InternetGatewayIDParameter"
	tagDefaultKeyRouteTableId           = "AvailabilityZone"
)

type AwsProvider struct {
	dry               bool
	natCidrBlocks     []string
	availabilityZones []string
	cloudformation    cloudformationiface.CloudFormationAPI
	ec2               ec2iface.EC2API
}

func NewAwsProvider(dry bool, natCidrBlocks, availabilityZones []string) *AwsProvider {
	p := defaultConfigProvider()
	return &AwsProvider{
		dry:               dry,
		natCidrBlocks:     natCidrBlocks,
		availabilityZones: availabilityZones,
		cloudformation:    cloudformation.New(p),
		ec2:               ec2.New(p),
	}
}

func (p AwsProvider) String() string {
	return ProviderName
}

func (p *AwsProvider) generateStackSpec(nets []string) (stackSpec, error) {
	spec := stackSpec{
		template:         p.generateTemplate(nets),
		tableID:          make(map[string]string),
		timeoutInMinutes: 10,
	}

	//get VPC
	vpcs, err := p.getVpcID()
	log.Debugf("%s: vpcs(%d)", p, len(vpcs))
	if err != nil {
		return spec, err
	}

	//get vpc ID from default vpc
	for _, vpc := range vpcs {
		if aws.BoolValue(vpc.IsDefault) {
			spec.vpcID = aws.StringValue(vpc.VpcId)
		}
	}

	//get assigned internet gateway
	igw, err := p.getInternetGatewayId(spec.vpcID)
	log.Debugf("%s: igw(%d)", p, len(igw))
	if err != nil {
		return spec, err
	}

	//get first internet gateway ID
	igwID := aws.StringValue(igw[0].InternetGatewayId)
	spec.internetGatewayID = igwID

	//get route tables
	rt, err := p.getRouteTables(spec.vpcID)
	log.Debugf("%s: rt(%d)", p, len(rt))
	if err != nil {
		return spec, err
	}

	// adding route tables to spec
	for _, table := range rt {
		for _, tag := range table.Tags {
			if tagDefaultKeyRouteTableId == aws.StringValue(tag.Key) {
				spec.tableID[aws.StringValue(tag.Value)] = aws.StringValue(table.RouteTableId)
			}
		}
	}
	return spec, nil
}

func (p *AwsProvider) Create(nets []string) error {
	log.Infof("%s: Create(%v)", p, nets)
	spec, err := p.generateStackSpec(nets)
	if err != nil {
		return fmt.Errorf("Failed to generate spec for create: %v", err)
	}

	stackID, err := p.createCFStack(nets, &spec)
	if err != nil {
		return fmt.Errorf("Failed to create CF stack: %v", err)
	}
	log.Infof("%s: Created CF Stack %s", p, stackID)
	return nil
}

func (p *AwsProvider) Update(nets []string) error {
	log.Infof("%s: Update(%v)", p, nets)
	spec, err := p.generateStackSpec(nets)
	if err != nil {
		return fmt.Errorf("Failed to generate spec for update: %v", err)
	}

	stackID, err := p.updateCFStack(nets, &spec)
	if err != nil {
		return fmt.Errorf("Failed to update CF stack: %v", err)
	}
	log.Infof("%s: Updated CF Stack %s", p, stackID)
	return nil
}

func (p *AwsProvider) Delete() error {
	log.Infof("%s Delete()", p)
	p.deleteCFStack()
	return nil
}

type stackSpec struct {
	name              string
	vpcID             string
	internetGatewayID string
	routeTableIDAZ1   string
	routeTableIDAZ2   string
	routeTableIDAZ3   string
	tableID           map[string]string
	timeoutInMinutes  uint
	template          string
}

func (p *AwsProvider) generateTemplate(nets []string) string {
	template := cft.NewTemplate()
	template.Parameters["VPCIDParameter"] = &cft.Parameter{
		Description: "VPC ID",
		Type:        "AWS::EC2::VPC::Id",
	}
	template.Parameters["InternetGatewayIDParameter"] = &cft.Parameter{
		Description: "Internet Gateway ID",
		Type:        "String",
	}

	for i, net := range nets {
		template.Parameters[fmt.Sprintf("DestinationCidrBlock%d", i+1)] = &cft.Parameter{
			Description: fmt.Sprintf("Destination CIDR Block %d", i+1),
			Type:        "String",
			Default:     net,
		}
	}

	for i := 1; i <= len(p.availabilityZones); i++ {
		template.Parameters[fmt.Sprintf("AZ%dRouteTableIDParameter", i)] = &cft.Parameter{
			Description: fmt.Sprintf(
				"Route Table ID Availability Zone %d", i),
			Type: "String",
		}
		template.AddResource(fmt.Sprintf("NATGateway%d", i), &cft.EC2NatGateway{
			SubnetId: cft.Ref(
				fmt.Sprintf("NATSubnet%d", i)).String(),
			AllocationId: cft.GetAtt(
				fmt.Sprintf("EIP%d", i), "AllocationId"),
		})
		template.AddResource(fmt.Sprintf("EIP%d", i), &cft.EC2EIP{
			Domain: cft.String("vpc"),
		})
		template.AddResource(fmt.Sprintf("NATSubnet%d", i), &cft.EC2Subnet{
			CidrBlock:        cft.String(p.natCidrBlocks[i-1]),
			AvailabilityZone: cft.String(p.availabilityZones[i-1]),
			VpcId:            cft.Ref("VPCIDParameter").String(),
			Tags: []cft.ResourceTag{
				cft.ResourceTag{
					Key: cft.String("Name"),
					Value: cft.String(
						fmt.Sprintf("nat-%s", p.availabilityZones[i-1])),
				},
			},
		})
		template.AddResource(fmt.Sprintf("NATSubnetRoute%d", i), &cft.EC2Route{
			RouteTableId: cft.Ref(
				fmt.Sprintf("NATSubnetRouteTable%d", i)).String(),
			DestinationCidrBlock: cft.String("0.0.0.0/0"),
			GatewayId:            cft.Ref("InternetGatewayIDParameter").String(),
		})
		template.AddResource(fmt.Sprintf("NATSubnetRouteTableAssociation%d", i), &cft.EC2SubnetRouteTableAssociation{
			RouteTableId: cft.Ref(
				fmt.Sprintf("NATSubnetRouteTable%d", i)).String(),
			SubnetId: cft.Ref(
				fmt.Sprintf("NATSubnet%d", i)).String(),
		})
		template.AddResource(fmt.Sprintf("NATSubnetRouteTable%d", i), &cft.EC2RouteTable{
			VpcId: cft.Ref("VPCIDParameter").String(),
		})
	}

	for j, cidrEntry := range nets {
		cleanCidrEntry := strings.Replace(cidrEntry, "/", "y", -1)
		cleanCidrEntry = strings.Replace(cleanCidrEntry, ".", "x", -1)
		for i := 1; i <= len(p.availabilityZones); i++ {
			template.AddResource("RouteToNAT"+cleanCidrEntry, &cft.EC2Route{
				RouteTableId: cft.Ref(
					fmt.Sprintf("AZ%dRouteTableIDParameter", i)).String(),
				DestinationCidrBlock: cft.Ref(
					fmt.Sprintf("DestinationCidrBlock%d", j+1)).String(),
				NatGatewayId: cft.Ref(
					fmt.Sprintf("NATGateway%d", i)).String(),
			})
		}
	}
	stack, _ := json.Marshal(template)
	return string(stack)
}

func (p *AwsProvider) deleteCFStack() error {
	params := &cloudformation.DeleteStackInput{StackName: aws.String(stackName)}
	if !p.dry {
		_, err := p.cloudformation.DeleteStack(params)
		return err
	}
	log.Debugf("%s: Stack to delete: %s", p, stackName)
	return nil
}

func (p *AwsProvider) updateCFStack(nets []string, spec *stackSpec) (string, error) {
	params := &cloudformation.UpdateStackInput{
		StackName: aws.String(stackName),
		Parameters: []*cloudformation.Parameter{
			cfParam(parameterVPCIDParameter, spec.vpcID),
			cfParam(parameterInternetGatewayIDParameter, spec.internetGatewayID),
		},
		TemplateBody: aws.String(p.generateTemplate(nets)),
	}
	for i, az := range p.availabilityZones {
		params.Parameters = append(params.Parameters,
			cfParam(
				fmt.Sprintf("AZ%dRouteTableIDParameter", i+1),
				spec.tableID[az]))
	}
	if !p.dry {
		resp, err := p.cloudformation.UpdateStack(params)
		if err != nil {
			return spec.name, err
		}
		return aws.StringValue(resp.StackId), nil
	}
	log.Debugf("%s: DRY: Stack to update: %s", p, params)
	return "DRY stackID", nil
}

func (p *AwsProvider) createCFStack(nets []string, spec *stackSpec) (string, error) {
	params := &cloudformation.CreateStackInput{
		StackName: aws.String(stackName),
		OnFailure: aws.String(cloudformation.OnFailureDelete),
		Parameters: []*cloudformation.Parameter{
			cfParam(parameterVPCIDParameter, spec.vpcID),
			cfParam(parameterInternetGatewayIDParameter, spec.internetGatewayID),
		},
		TemplateBody:     aws.String(spec.template),
		TimeoutInMinutes: aws.Int64(int64(spec.timeoutInMinutes)),
	}
	for i, az := range p.availabilityZones {
		params.Parameters = append(params.Parameters,
			cfParam(
				fmt.Sprintf("AZ%dRouteTableIDParameter", i+1),
				spec.tableID[az]))
	}
	if !p.dry {
		resp, err := p.cloudformation.CreateStack(params)
		if err != nil {
			return spec.name, err
		}
		return aws.StringValue(resp.StackId), nil
	}
	log.Debugf("%s: DRY: Stack to create: %s", p, params)
	return "DRY stackID", nil

}

func defaultConfigProvider() client.ConfigProvider {
	cfg := aws.NewConfig().WithMaxRetries(3)
	cfg = cfg.WithHTTPClient(instrumented_http.NewClient(cfg.HTTPClient, nil))
	opts := session.Options{
		SharedConfigState: session.SharedConfigEnable,
		Config:            *cfg,
	}
	return session.Must(session.NewSessionWithOptions(opts))
}

func cfParam(key, value string) *cloudformation.Parameter {
	return &cloudformation.Parameter{
		ParameterKey:   aws.String(key),
		ParameterValue: aws.String(value),
	}
}

func (p *AwsProvider) getInternetGatewayId(vpcID string) ([]*ec2.InternetGateway, error) {
	params := &ec2.DescribeInternetGatewaysInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("attachment.vpc-id"),
				Values: []*string{
					aws.String(vpcID),
				},
			},
		},
	}
	resp, err := p.ec2.DescribeInternetGateways(params)
	if err != nil {
		return nil, err
	}
	return resp.InternetGateways, nil
}

func (p *AwsProvider) getVpcID() ([]*ec2.Vpc, error) {
	params := &ec2.DescribeVpcsInput{}
	resp, err := p.ec2.DescribeVpcs(params)
	if err != nil {
		return nil, err
	}
	return resp.Vpcs, nil
}

func (p *AwsProvider) getRouteTables(vpcID string) ([]*ec2.RouteTable, error) {
	params := &ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("vpc-id"),
				Values: []*string{
					aws.String(vpcID),
				},
			},
			{
				Name: aws.String("tag-key"),
				Values: []*string{
					aws.String(tagDefaultKeyRouteTableId),
				},
			},
		},
	}
	resp, err := p.ec2.DescribeRouteTables(params)
	if err != nil {
		return nil, err
	}
	return resp.RouteTables, nil
}
