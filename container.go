package emongo

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gotomicro/ego/core/eapp"
	"github.com/gotomicro/ego/core/econf"
	"github.com/gotomicro/ego/core/elog"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.opentelemetry.io/contrib/instrumentation/go.mongodb.org/mongo-driver/mongo/otelmongo"
)

type Option func(c *Container)

type Container struct {
	config *config
	name   string
	logger *elog.Component
}

// DefaultContainer 返回默认Container
func DefaultContainer() *Container {
	return &Container{
		config: DefaultConfig(),
		logger: elog.EgoLogger.With(elog.FieldComponent(PackageName)),
	}
}

// Load 载入配置，初始化Container
func Load(key string) *Container {
	c := DefaultContainer()
	if err := econf.UnmarshalKey(key, &c.config); err != nil {
		c.logger.Panic("parse config error", elog.FieldErr(err), elog.FieldKey(key))
		return c
	}

	c.logger = c.logger.With(elog.FieldComponentName(key))
	c.name = key
	return c
}

func (c *Container) newSession(config config) *Client {
	// check config param
	c.isConfigErr(config)

	clientOpts := options.Client()

	// 加载 TLS 配置
	err := config.Authentication.ConfigureAuthentication(clientOpts)
	if err != nil {
		c.logger.Panic("mongo TLS configuration", elog.Any("authentication", config.Authentication), elog.Any("error", err))
	}

	if c.config.EnableTraceInterceptor {
		clientOpts.Monitor = otelmongo.NewMonitor()
	}
	clientOpts.SetSocketTimeout(config.SocketTimeout)
	clientOpts.SetMaxPoolSize(uint64(config.MaxPoolSize))
	clientOpts.SetMinPoolSize(uint64(config.MinPoolSize))
	clientOpts.SetMaxConnIdleTime(config.MaxConnIdleTime)

	ctx, cancel := context.WithTimeout(context.Background(), config.DialTimeout)
	defer cancel()

	client, err := Connect(ctx, clientOpts.ApplyURI(config.DSN))
	if err != nil {
		c.logger.Panic("dial mongo", elog.FieldAddr(config.DSN), elog.Any("error", err))
	}
	if eapp.IsDevelopmentMode() || c.config.Debug {
		client.logMode = true
	}

	// 必须加入ping包，否则账号问题，需要发报文才能发现问题
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = client.Ping(ctx, readpref.Primary())
	if err != nil {
		c.logger.Panic("dial mongo", elog.FieldAddr(config.DSN), elog.Any("error", err))
	}

	instances.Store(c.name, client)
	client.wrapProcessor(InterceptorChain(config.interceptors...))
	return client
}

var instances = sync.Map{}

func iterate(fn func(name string, db *mongo.Client) bool) {
	instances.Range(func(key, val interface{}) bool {
		return fn(key.(string), val.(*mongo.Client))
	})
}

func get(name string) *mongo.Client {
	if ins, ok := instances.Load(name); ok {
		return ins.(*mongo.Client)
	}
	return nil
}

func (c *Container) isConfigErr(config config) {
	if config.SocketTimeout == time.Duration(0) {
		c.logger.Panic("invalid config", elog.FieldExtMessage("socketTimeout"))
	}
}

// Build 构建Container
func (c *Container) Build(options ...Option) *Component {
	if options == nil {
		options = make([]Option, 0)
	}
	if c.config.Debug {
		options = append(options, WithInterceptor(debugInterceptor(c.name, c.config)))
	}
	if c.config.EnableMetricInterceptor {
		options = append(options, WithInterceptor(metricInterceptor(c.name, c.config, c.logger)))
	}
	if c.config.EnableAccessInterceptor {
		options = append(options, WithInterceptor(accessInterceptor(c.name, c.config, c.logger)))
	}
	if c.config.EnableTraceInterceptor {
		// options = append(options, WithInterceptor(traceInterceptor(c.name, c.config, c.logger)))
	}
	for _, option := range options {
		option(c)
	}

	c.logger = c.logger.With(elog.FieldAddr(fmt.Sprintf("%s", c.config.DSN)))
	client := c.newSession(*c.config)
	return &Component{
		config: c.config,
		client: client,
		logger: c.logger,
	}
}
