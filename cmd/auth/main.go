package main

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/vardius/gocontainer"
	pushpullproto "github.com/vardius/pushpull/proto"
	"google.golang.org/grpc"
	grpchealth "google.golang.org/grpc/health"
	oauth2models "gopkg.in/oauth2.v4/models"

	"github.com/vardius/go-api-boilerplate/cmd/auth/internal/application/config"
	"github.com/vardius/go-api-boilerplate/cmd/auth/internal/application/eventhandler"
	"github.com/vardius/go-api-boilerplate/cmd/auth/internal/application/oauth2"
	"github.com/vardius/go-api-boilerplate/cmd/auth/internal/domain/client"
	"github.com/vardius/go-api-boilerplate/cmd/auth/internal/domain/token"
	persistence "github.com/vardius/go-api-boilerplate/cmd/auth/internal/infrastructure/persistence/mysql"
	"github.com/vardius/go-api-boilerplate/cmd/auth/internal/infrastructure/repository"
	authgrpc "github.com/vardius/go-api-boilerplate/cmd/auth/internal/interfaces/grpc"
	authhttp "github.com/vardius/go-api-boilerplate/cmd/auth/internal/interfaces/http"
	"github.com/vardius/go-api-boilerplate/pkg/application"
	"github.com/vardius/go-api-boilerplate/pkg/auth"
	"github.com/vardius/go-api-boilerplate/pkg/buildinfo"
	"github.com/vardius/go-api-boilerplate/pkg/commandbus/memory"
	"github.com/vardius/go-api-boilerplate/pkg/eventbus"
	"github.com/vardius/go-api-boilerplate/pkg/eventbus/pushpull"
	eventstore "github.com/vardius/go-api-boilerplate/pkg/eventstore/mysql"
	grpcutils "github.com/vardius/go-api-boilerplate/pkg/grpc"
	"github.com/vardius/go-api-boilerplate/pkg/log"
	"github.com/vardius/go-api-boilerplate/pkg/mysql"
)

func init() {
	rand.Seed(time.Now().UnixNano())

	gocontainer.GlobalContainer = nil // disable global container instance
}

func main() {
	buildinfo.PrintVersionOrContinue()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.New(config.Env.App.Environment)
	grpcServer := grpcutils.NewServer(
		grpcutils.ServerConfig{
			ServerMinTime: config.Env.GRPC.ServerMinTime,
			ServerTime:    config.Env.GRPC.ServerTime,
			ServerTimeout: config.Env.GRPC.ServerTimeout,
		},
		logger,
		nil,
		nil,
	)
	commandBus := memory.New(config.Env.CommandBus.QueueSize, logger)

	mysqlConnection := mysql.NewConnection(
		ctx,
		mysql.ConnectionConfig{
			Host:            config.Env.MYSQL.Host,
			Port:            config.Env.MYSQL.Port,
			User:            config.Env.MYSQL.User,
			Pass:            config.Env.MYSQL.Pass,
			Database:        config.Env.MYSQL.Database,
			ConnMaxLifetime: config.Env.MYSQL.ConnMaxLifetime,
			MaxIdleConns:    config.Env.MYSQL.MaxIdleConns,
			MaxOpenConns:    config.Env.MYSQL.MaxOpenConns,
		},
		logger,
	)
	defer mysqlConnection.Close()
	grpcPushPullConn := grpcutils.NewConnection(
		ctx,
		config.Env.PushPull.Host,
		config.Env.GRPC.Port,
		grpcutils.ConnectionConfig{
			ConnTime:    config.Env.GRPC.ConnTime,
			ConnTimeout: config.Env.GRPC.ConnTimeout,
		},
		logger,
	)
	defer grpcPushPullConn.Close()
	grpcAuthConn := grpcutils.NewConnection(
		ctx,
		config.Env.GRPC.Host,
		config.Env.GRPC.Port,
		grpcutils.ConnectionConfig{
			ConnTime:    config.Env.GRPC.ConnTime,
			ConnTimeout: config.Env.GRPC.ConnTimeout,
		},
		logger,
	)
	defer grpcAuthConn.Close()

	eventStore := eventstore.New(mysqlConnection)
	grpPushPullClient := pushpullproto.NewPushPullClient(grpcPushPullConn)
	eventBus := pushpull.New(config.Env.App.EventHandlerTimeout, grpPushPullClient, logger)
	tokenRepository := repository.NewTokenRepository(eventStore, eventBus)
	clientRepository := repository.NewClientRepository(eventStore, eventBus)
	tokenPersistenceRepository := persistence.NewTokenRepository(mysqlConnection)
	clientPersistenceRepository := persistence.NewClientRepository(mysqlConnection)
	userPersistenceRepository := persistence.NewUserRepository(mysqlConnection)
	tokenStore := oauth2.NewTokenStore(tokenPersistenceRepository, commandBus)
	clientStore := oauth2.NewClientStore(clientPersistenceRepository)
	authenticator := auth.NewSecretAuthenticator([]byte(config.Env.App.Secret))
	manager := oauth2.NewManager(tokenStore, clientStore, authenticator, userPersistenceRepository)
	oauth2Server := oauth2.InitServer(manager, mysqlConnection, logger, config.Env.App.Secret, config.Env.OAuth.InitTimeout)
	grpcHealthServer := grpchealth.NewServer()
	grpcAuthServer := authgrpc.NewServer(oauth2Server, authenticator, logger)
	router := authhttp.NewRouter(
		logger,
		oauth2Server,
		mysqlConnection,
		map[string]*grpc.ClientConn{
			"pushpull": grpcPushPullConn,
			"auth":     grpcAuthConn,
		},
	)
	app := application.New(logger)

	// store our internal user service client
	if err := clientStore.SetInternal(config.Env.User.ClientID, &oauth2models.Client{
		ID:     config.Env.User.ClientID,
		Secret: config.Env.User.ClientSecret,
		Domain: fmt.Sprintf("http://%s:%d", config.Env.User.Host, config.Env.HTTP.Port),
	}); err != nil {
		panic(err)
	}

	if err := commandBus.Subscribe(ctx, (token.Create{}).GetName(), token.OnCreate(tokenRepository, mysqlConnection)); err != nil {
		panic(err)
	}
	if err := commandBus.Subscribe(ctx, (token.Remove{}).GetName(), token.OnRemove(tokenRepository, mysqlConnection)); err != nil {
		panic(err)
	}
	if err := commandBus.Subscribe(ctx, (client.Create{}).GetName(), client.OnCreate(clientRepository, mysqlConnection)); err != nil {
		panic(err)
	}
	if err := commandBus.Subscribe(ctx, (client.Remove{}).GetName(), client.OnRemove(clientRepository, mysqlConnection)); err != nil {
		panic(err)
	}

	go func() {
		go func() {
			eventbus.RegisterGRPCHandlers(
				"pushpull",
				grpcPushPullConn,
				eventBus,
				map[string]eventbus.EventHandler{
					(token.WasCreated{}).GetType():  eventhandler.WhenTokenWasCreated(mysqlConnection, tokenPersistenceRepository),
					(token.WasRemoved{}).GetType():  eventhandler.WhenTokenWasRemoved(mysqlConnection, tokenPersistenceRepository),
					(client.WasCreated{}).GetType(): eventhandler.WhenClientWasCreated(mysqlConnection, clientPersistenceRepository),
					(client.WasRemoved{}).GetType(): eventhandler.WhenClientWasRemoved(mysqlConnection, clientPersistenceRepository),
				},
				5*time.Second,
			)
		}()
	}()

	app.AddAdapters(
		authhttp.NewAdapter(
			fmt.Sprintf("%s:%d", config.Env.HTTP.Host, config.Env.HTTP.Port),
			router,
		),
		authgrpc.NewAdapter(
			fmt.Sprintf("%s:%d", config.Env.GRPC.Host, config.Env.GRPC.Port),
			grpcServer,
			grpcHealthServer,
			grpcAuthServer,
		),
	)

	if config.Env.App.Environment == "development" {
		app.AddAdapters(
			application.NewDebugAdapter(
				fmt.Sprintf("%s:%d", config.Env.Debug.Host, config.Env.Debug.Port),
			),
		)
	}

	app.WithShutdownTimeout(config.Env.App.ShutdownTimeout)
	app.Run(ctx)
}
