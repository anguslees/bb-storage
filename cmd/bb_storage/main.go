package main

import (
	"log"
	"os"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-storage/pkg/auth"
	"github.com/buildbarn/bb-storage/pkg/blobstore"
	blobstore_configuration "github.com/buildbarn/bb-storage/pkg/blobstore/configuration"
	"github.com/buildbarn/bb-storage/pkg/blobstore/grpcservers"
	"github.com/buildbarn/bb-storage/pkg/builder"
	"github.com/buildbarn/bb-storage/pkg/capabilities"
	"github.com/buildbarn/bb-storage/pkg/global"
	bb_grpc "github.com/buildbarn/bb-storage/pkg/grpc"
	"github.com/buildbarn/bb-storage/pkg/proto/configuration/bb_storage"
	"github.com/buildbarn/bb-storage/pkg/proto/icas"
	"github.com/buildbarn/bb-storage/pkg/proto/iscc"
	"github.com/buildbarn/bb-storage/pkg/util"

	"google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("Usage: bb_storage bb_storage.jsonnet")
	}
	var configuration bb_storage.ApplicationConfiguration
	if err := util.UnmarshalConfigurationFromFile(os.Args[1], &configuration); err != nil {
		log.Fatalf("Failed to read configuration from %s: %s", os.Args[1], err)
	}
	lifecycleState, grpcClientFactory, err := global.ApplyConfiguration(configuration.Global)
	if err != nil {
		log.Fatal("Failed to apply global configuration options: ", err)
	}

	// Providers for data returned by ServerCapabilities.cache_capabilities
	// as part of the GetCapabilities() call. We permit these calls
	// if the client is permitted to at least one method against one
	// of the data stores described in REv2.
	var cacheCapabilitiesProviders []capabilities.Provider
	var cacheCapabilitiesAuthorizers []auth.Authorizer

	// Storage access.
	contentAddressableStorage, actionCache, err := blobstore_configuration.NewCASAndACBlobAccessFromConfiguration(
		configuration.Blobstore,
		grpcClientFactory,
		int(configuration.MaximumMessageSizeBytes))
	if err != nil {
		log.Fatal(err)
	}

	// TODO: Make Content Addressable Storage support optional.
	if true {
		getAuthorizer, putAuthorizer, findMissingAuthorizer, err := newScannableBlobAccessAuthorizers(configuration.ContentAddressableStorageAuthorizers)
		if err != nil {
			log.Fatal("Failed to create Content Addressable Storage authorizers: ", err)
		}
		cacheCapabilitiesProviders = append(
			cacheCapabilitiesProviders,
			contentAddressableStorage)
		cacheCapabilitiesAuthorizers = append(
			cacheCapabilitiesAuthorizers,
			getAuthorizer,
			putAuthorizer,
			findMissingAuthorizer)
		contentAddressableStorage = blobstore.NewAuthorizingBlobAccess(
			contentAddressableStorage,
			getAuthorizer,
			putAuthorizer,
			findMissingAuthorizer)
	}

	// TODO: Make Action Cache support optional.
	if true {
		getAuthorizer, putAuthorizer, err := newNonScannableBlobAccessAuthorizers(configuration.ActionCacheAuthorizers)
		if err != nil {
			log.Fatal("Failed to create Action Cache authorizers: ", err)
		}
		cacheCapabilitiesProviders = append(
			cacheCapabilitiesProviders,
			capabilities.NewActionCacheUpdateEnabledClearingProvider(actionCache, putAuthorizer))
		cacheCapabilitiesAuthorizers = append(
			cacheCapabilitiesAuthorizers,
			getAuthorizer,
			putAuthorizer)
		actionCache = blobstore.NewAuthorizingBlobAccess(
			actionCache,
			getAuthorizer,
			putAuthorizer,
			nil)
	}

	// Buildbarn extension: Indirect Content Addressable Storage
	// (ICAS) access.
	var indirectContentAddressableStorage blobstore.BlobAccess
	if configuration.IndirectContentAddressableStorage != nil {
		info, err := blobstore_configuration.NewBlobAccessFromConfiguration(
			configuration.IndirectContentAddressableStorage,
			blobstore_configuration.NewICASBlobAccessCreator(
				grpcClientFactory,
				int(configuration.MaximumMessageSizeBytes)))
		if err != nil {
			log.Fatal("Failed to create Indirect Content Addressable Storage: ", err)
		}

		icasGetAuthorizer, icasPutAuthorizer, icasFindMissingAuthorizer, err := newScannableBlobAccessAuthorizers(configuration.IndirectContentAddressableStorageAuthorizers)
		if err != nil {
			log.Fatal("Failed to create Indirect Content Addressable Storage authorizer: ", err)
		}
		indirectContentAddressableStorage = blobstore.NewAuthorizingBlobAccess(
			info.BlobAccess,
			icasGetAuthorizer,
			icasPutAuthorizer,
			icasFindMissingAuthorizer)
	}

	// Buildbarn extension: Initial Size Class Cache (ISCC).
	var initialSizeClassCache blobstore.BlobAccess
	if configuration.InitialSizeClassCache != nil {
		info, err := blobstore_configuration.NewBlobAccessFromConfiguration(
			configuration.InitialSizeClassCache,
			blobstore_configuration.NewISCCBlobAccessCreator(
				grpcClientFactory,
				int(configuration.MaximumMessageSizeBytes)))
		if err != nil {
			log.Fatal("Failed to create Initial Size Class Cache: ", err)
		}

		isccGetAuthorizer, isccPutAuthorizer, err := newNonScannableBlobAccessAuthorizers(configuration.InitialSizeClassCacheAuthorizers)
		if err != nil {
			log.Fatal("Failed to create Initial Size Class Cache authorizer: ", err)
		}
		initialSizeClassCache = blobstore.NewAuthorizingBlobAccess(
			info.BlobAccess,
			isccGetAuthorizer,
			isccPutAuthorizer,
			nil)
	}

	var capabilitiesProviders []capabilities.Provider
	if len(cacheCapabilitiesProviders) > 0 {
		capabilitiesProviders = append(
			capabilitiesProviders,
			capabilities.NewAuthorizingProvider(
				capabilities.NewMergingProvider(cacheCapabilitiesProviders),
				auth.NewAnyAuthorizer(cacheCapabilitiesAuthorizers)))
	}

	// Create a demultiplexing build queue that forwards traffic to
	// one or more schedulers specified in the configuration file.
	var buildQueue builder.BuildQueue
	if len(configuration.Schedulers) > 0 {
		baseBuildQueue, err := builder.NewDemultiplexingBuildQueueFromConfiguration(configuration.Schedulers, grpcClientFactory)
		if err != nil {
			log.Fatal(err)
		}
		executeAuthorizer, err := auth.DefaultAuthorizerFactory.NewAuthorizerFromConfiguration(configuration.GetExecuteAuthorizer())
		if err != nil {
			log.Fatal("Failed to create execute authorizer: ", err)
		}
		buildQueue = builder.NewAuthorizingBuildQueue(baseBuildQueue, executeAuthorizer)
		capabilitiesProviders = append(capabilitiesProviders, buildQueue)
	}

	go func() {
		log.Fatal(
			"gRPC server failure: ",
			bb_grpc.NewServersFromConfigurationAndServe(
				configuration.GrpcServers,
				func(s grpc.ServiceRegistrar) {
					if contentAddressableStorage != nil {
						remoteexecution.RegisterContentAddressableStorageServer(
							s,
							grpcservers.NewContentAddressableStorageServer(
								contentAddressableStorage,
								configuration.MaximumMessageSizeBytes))
						bytestream.RegisterByteStreamServer(
							s,
							grpcservers.NewByteStreamServer(
								contentAddressableStorage,
								1<<16))
					}
					if actionCache != nil {
						remoteexecution.RegisterActionCacheServer(
							s,
							grpcservers.NewActionCacheServer(
								actionCache,
								int(configuration.MaximumMessageSizeBytes)))
					}
					if indirectContentAddressableStorage != nil {
						icas.RegisterIndirectContentAddressableStorageServer(
							s,
							grpcservers.NewIndirectContentAddressableStorageServer(
								indirectContentAddressableStorage,
								int(configuration.MaximumMessageSizeBytes)))
					}
					if initialSizeClassCache != nil {
						iscc.RegisterInitialSizeClassCacheServer(
							s,
							grpcservers.NewInitialSizeClassCacheServer(
								initialSizeClassCache,
								int(configuration.MaximumMessageSizeBytes)))
					}
					if buildQueue != nil {
						remoteexecution.RegisterExecutionServer(s, buildQueue)
					}
					if len(capabilitiesProviders) > 0 {
						remoteexecution.RegisterCapabilitiesServer(
							s,
							capabilities.NewServer(
								capabilities.NewMergingProvider(capabilitiesProviders)))
					}
				}))
	}()

	lifecycleState.MarkReadyAndWait()
}

func newNonScannableBlobAccessAuthorizers(configuration *bb_storage.NonScannableAuthorizersConfiguration) (auth.Authorizer, auth.Authorizer, error) {
	getAuthorizer, err := auth.DefaultAuthorizerFactory.NewAuthorizerFromConfiguration(configuration.GetGet())
	if err != nil {
		return nil, nil, util.StatusWrap(err, "Failed to create Get() authorizer")
	}

	putAuthorizer, err := auth.DefaultAuthorizerFactory.NewAuthorizerFromConfiguration(configuration.GetPut())
	if err != nil {
		return nil, nil, util.StatusWrap(err, "Failed to create Put() authorizer")
	}

	return getAuthorizer, putAuthorizer, nil
}

func newScannableBlobAccessAuthorizers(configuration *bb_storage.ScannableAuthorizersConfiguration) (auth.Authorizer, auth.Authorizer, auth.Authorizer, error) {
	getAuthorizer, err := auth.DefaultAuthorizerFactory.NewAuthorizerFromConfiguration(configuration.GetGet())
	if err != nil {
		return nil, nil, nil, util.StatusWrap(err, "Failed to create Get() authorizer")
	}

	putAuthorizer, err := auth.DefaultAuthorizerFactory.NewAuthorizerFromConfiguration(configuration.GetPut())
	if err != nil {
		return nil, nil, nil, util.StatusWrap(err, "Failed to create Put() authorizer")
	}

	findMissingAuthorizer, err := auth.DefaultAuthorizerFactory.NewAuthorizerFromConfiguration(configuration.GetFindMissing())
	if err != nil {
		return nil, nil, nil, util.StatusWrap(err, "Failed to create FindMissing() authorizer")
	}

	return getAuthorizer, putAuthorizer, findMissingAuthorizer, nil
}
