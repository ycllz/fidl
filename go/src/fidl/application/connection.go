// Copyright 2015 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package application

import (
	"log"

	"fidl/bindings"
	"fidl/system"

	sp "mojo/public/interfaces/application/service_provider"
	"mojo/public/interfaces/bindings/service_describer"
)

type connectionInfo struct {
	requestorURL  string
	connectionURL string
}

// RequestorURL returns the URL of application that established the connection.
func (c *connectionInfo) RequestorURL() string {
	return c.requestorURL
}

// ConnectionURL returns the URL that was used by the source application to
// establish a connection to the destination application.
func (c *connectionInfo) ConnectionURL() string {
	return c.connectionURL
}

// ServiceRequest is an interface request for a specified mojo service.
type ServiceRequest interface {
	// Name returns the name of requested mojo service.
	Name() string

	// ServiceDescription returns a service description, which can be queried to
	// examine the type information of the service associated with this
	// ServiceRequest.
	// Note: In some implementations, the ServiceDescription returned will not
	// provide type information. Methods called may return nil or an error.
	ServiceDescription() service_describer.ServiceDescription

	// PassChannel passes ownership of the underlying message pipe
	// handle to the newly created handle object, invalidating the
	// underlying handle object in the process.
	PassChannel() system.ChannelHandle
}

// ServiceFactory provides implementation of a mojo service.
type ServiceFactory interface {
	// Name returns the name of provided mojo service.
	Name() string

	// ServiceDescription returns a service description, which can be queried to
	// examine the type information of the mojo service associated with this
	// ServiceFactory.
	// Note: In some implementations, the ServiceDescription returned will not
	// provide type information. Methods called may return nil or an error.
	ServiceDescription() service_describer.ServiceDescription

	// Create binds an implementation of mojo service to the provided
	// message pipe and runs it.
	Create(pipe system.ChannelHandle)
}

// Connection represents a connection to another application. An instance of
// this struct is passed to Delegate's AcceptConnection() function each time a
// connection is made to this application.
// TODO(vtl): This is largely overkill now that we no longer have "wrong way"
// service providers (a.k.a. "exposed services"). Things should be simplified.
// https://github.com/domokit/mojo/issues/762
type Connection struct {
	connectionInfo
	// Request for local services. Is valid until ProvideServices is called.
	servicesRequest *sp.ServiceProvider_Request
	// Indicates that ProvideServices function was already called.
	servicesProvided   bool
	localServices      *bindings.Stub
	outgoingConnection *OutgoingConnection
	isClosed           bool
	// Is set if ProvideServicesWithDescriber was called.
	// Note: When DescribeServices is invoked, some implementations may return
	// incomplete ServiceDescriptions. For example, if type information was not
	// generated, then the methods called may return nil or an error.
	describer *ServiceDescriberFactory
}

func newConnection(requestorURL string, services sp.ServiceProvider_Request, resolvedURL string) *Connection {
	info := connectionInfo{
		requestorURL,
		resolvedURL,
	}
	return &Connection{
		connectionInfo:  info,
		servicesRequest: &services,
		outgoingConnection: &OutgoingConnection{
			info,
			nil,
		},
	}
}

// ProvideServices starts a service provider on a separate goroutine that
// provides given services to the remote application. Returns a pointer to
// outgoing connection that can be used to connect to services provided by
// remote application.
// Panics if called more than once.
func (c *Connection) ProvideServices(services ...ServiceFactory) *OutgoingConnection {
	if c.servicesProvided {
		panic("ProvideServices or ProvideServicesWithDescriber can be called only once")
	}
	c.servicesProvided = true
	if c.servicesRequest == nil {
		return c.outgoingConnection
	}
	if len(services) == 0 {
		c.servicesRequest.PassChannel().Close()
		return c.outgoingConnection
	}

	provider := &serviceProviderImpl{
		make(map[string]ServiceFactory),
	}
	for _, service := range services {
		provider.AddService(service)
	}
	c.localServices = sp.NewServiceProviderStub(*c.servicesRequest, provider, bindings.GetAsyncWaiter())
	go func() {
		for {
			if err := c.localServices.ServeRequest(); err != nil {
				connectionError, ok := err.(*bindings.ConnectionError)
				if !ok || !connectionError.Closed() {
					log.Println(err)
				}
				break
			}
		}
	}()
	return c.outgoingConnection
}

// ProvideServicesWithDescriber is an alternative to ProvideServices that, in
// addition to providing the given services, also provides type descriptions of
// the given services. See ProvideServices for a description of what it does.
// This method will invoke ProvideServices after appending the ServiceDescriber
// service to |services|. See service_describer.mojom for a description of the
// ServiceDescriber interface. Client Mojo applications can choose to connect
// to this ServiceDescriber interface, which describes the other services listed
// in |services|.
// Note that the implementation of ServiceDescriber will make the optional
// DeclarationData available on all types, and in particular, the names used in
// .mojom files will be exposed to client applications.
func (c *Connection) ProvideServicesWithDescriber(services ...ServiceFactory) *OutgoingConnection {
	if c.servicesProvided {
		panic("ProvideServices or ProvideServicesWithDescriber can be called only once")
	}
	mapping := make(map[string]service_describer.ServiceDescription)
	for _, service := range services {
		mapping[service.Name()] = service.ServiceDescription()
	}
	c.describer = newServiceDescriberFactory(mapping)
	servicesWithDescriber := append(services, &service_describer.ServiceDescriber_ServiceFactory{c.describer})

	return c.ProvideServices(servicesWithDescriber...)
}

// Close closes both incoming and outgoing parts of the connection.
func (c *Connection) Close() {
	if c.servicesRequest != nil {
		c.servicesRequest.Close()
	}
	if c.localServices != nil {
		c.localServices.Close()
	}
	if c.describer != nil {
		c.describer.Close()
	}
	if c.outgoingConnection.remoteServices != nil {
		c.outgoingConnection.remoteServices.Close_Proxy()
	}
	c.isClosed = true
}

// OutgoingConnection represents outgoing part of connection to another
// application. In order to close it close the |Connection| object that returned
// this |OutgoingConnection|.
type OutgoingConnection struct {
	connectionInfo
	remoteServices *sp.ServiceProvider_Proxy
}

// ConnectToService asks remote application to provide a service through the
// message pipe endpoint supplied by the caller.
func (c *OutgoingConnection) ConnectToService(request ServiceRequest) {
	pipe := request.PassChannel()
	if c.remoteServices == nil {
		pipe.Close()
		return
	}
	c.remoteServices.ConnectToService(request.Name(), pipe)
}

// serviceProviderImpl is an implementation of mojo ServiceProvider interface.
type serviceProviderImpl struct {
	factories map[string]ServiceFactory
}

// Mojo ServiceProvider implementation.
func (sp *serviceProviderImpl) ConnectToService(name string, messagePipe system.ChannelHandle) error {
	factory, ok := sp.factories[name]
	if !ok {
		messagePipe.Close()
		return nil
	}
	factory.Create(messagePipe)
	return nil
}

func (sp *serviceProviderImpl) AddService(factory ServiceFactory) {
	sp.factories[factory.Name()] = factory
}
