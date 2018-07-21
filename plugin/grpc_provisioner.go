package plugin

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/hashicorp/terraform/configs/configschema"
	"github.com/hashicorp/terraform/plugin/proto"
	"github.com/hashicorp/terraform/provisioners"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/msgpack"
	"google.golang.org/grpc"
)

// provisioners.Interface grpc implementation
type GRPCProvisioner struct {
	conn   *grpc.ClientConn
	client proto.ProvisionerClient
	ctx    context.Context

	// Cache the schema since we need it for serialization in each method call.
	mu     sync.Mutex
	schema *configschema.Block
}

func (p *GRPCProvisioner) GetSchema() (resp provisioners.GetSchemaResponse) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.schema != nil {
		return provisioners.GetSchemaResponse{
			Provisioner: p.schema,
		}
	}

	protoResp, err := p.client.GetSchema(p.ctx, new(proto.GetProvisionerSchema_Request))
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	if protoResp.Provisioner == nil {
		resp.Diagnostics = resp.Diagnostics.Append(errors.New("missing provisioner schema"))
		return resp
	}

	resp.Provisioner = schemaBlock(protoResp.Provisioner.Block)

	p.schema = resp.Provisioner

	return resp
}

func (p *GRPCProvisioner) ValidateProvisionerConfig(r provisioners.ValidateProvisionerConfigRequest) (resp provisioners.ValidateProvisionerConfigResponse) {
	schema := p.GetSchema()
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = resp.Diagnostics.Append(schema.Diagnostics)
		return resp
	}

	mp, err := msgpack.Marshal(r.Config, schema.Provisioner.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoReq := &proto.ValidateProvisionerConfig_Request{
		Config: &proto.DynamicValue{Msgpack: mp},
	}
	protoResp, err := p.client.ValidateProvisionerConfig(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	resp.Diagnostics = resp.Diagnostics.Append(ProtoToDiagnostics(protoResp.Diagnostics))
	return resp
}

func (p *GRPCProvisioner) ProvisionResource(r provisioners.ProvisionResourceRequest) (resp provisioners.ProvisionResourceResponse) {
	schema := p.GetSchema()
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = resp.Diagnostics.Append(schema.Diagnostics)
		return resp
	}

	mp, err := msgpack.Marshal(r.Config, schema.Provisioner.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	// connection is always assumed to be a simple string map
	connMP, err := msgpack.Marshal(r.Connection, cty.Map(cty.String))
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoReq := &proto.ProvisionResource_Request{
		Config:     &proto.DynamicValue{Msgpack: mp},
		Connection: &proto.DynamicValue{Msgpack: connMP},
	}

	outputClient, err := p.client.ProvisionResource(p.ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	for {
		rcv, err := outputClient.Recv()
		if rcv != nil {
			r.UIOutput.Output(rcv.Output)
		}
		if err != nil {
			if err != io.EOF {
				resp.Diagnostics = resp.Diagnostics.Append(err)
			}
			break
		}

		if len(rcv.Diagnostics) > 0 {
			resp.Diagnostics = resp.Diagnostics.Append(ProtoToDiagnostics(rcv.Diagnostics))
			break
		}
	}

	return resp
}

func (p *GRPCProvisioner) Stop() error {
	protoResp, err := p.client.Stop(p.ctx, &proto.Stop_Request{})
	if err != nil {
		return err
	}
	if protoResp.Error != "" {
		return errors.New(protoResp.Error)
	}
	return nil
}

func (p *GRPCProvisioner) Close() error {
	return nil
}