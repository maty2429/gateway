package users

import (
	"context"

	"google.golang.org/grpc"
)

// UserServiceClient is the client API for UserService service.
type UserServiceClient interface {
	GetUser(ctx context.Context, in *GetUserRequest, opts ...grpc.CallOption) (*GetUserResponse, error)
	CreateUser(ctx context.Context, in *CreateUserRequest, opts ...grpc.CallOption) (*CreateUserResponse, error)
}

type dummyUserServiceClient struct{}

func (d *dummyUserServiceClient) GetUser(ctx context.Context, in *GetUserRequest, opts ...grpc.CallOption) (*GetUserResponse, error) {
	return nil, grpc.ErrClientConnClosing
}

func (d *dummyUserServiceClient) CreateUser(ctx context.Context, in *CreateUserRequest, opts ...grpc.CallOption) (*CreateUserResponse, error) {
	return nil, grpc.ErrClientConnClosing
}

// NewUserServiceClient returns a dummy client since actual gRPC upstreams are compiled out-of-band.
func NewUserServiceClient(cc grpc.ClientConnInterface) UserServiceClient {
	return &dummyUserServiceClient{}
}
