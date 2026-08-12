// Container Apps hosting the two pieces that cannot run on Azure Functions.
//
// 1. fleet-telemetry — Tesla vehicles open a *mutual-TLS WebSocket* to this server and stream
//    protobuf records. mTLS has to terminate at the process (the server reads the peer certificate
//    off the TLS state to identify the vehicle), which is why the environment is VNet-integrated
//    and ingress is raw TCP rather than the managed HTTP front end.
//    A sidecar bridges records to the Function's HTTP endpoint, because fleet-telemetry only
//    dispatches to kafka/kinesis/pubsub/zmq/redis/mqtt — there is no HTTP dispatcher.
//
// 2. tesla-http-proxy — signs vehicle commands with the app's private key. Vehicles built after
//    2021 reject unsigned commands, so set_charging_amps cannot be sent straight to Fleet API.
//
// Both scale to zero and sit inside the Container Apps monthly free grant at this request volume.

@description('Prefix for resource names.')
param namePrefix string

@description('Azure region.')
param location string

param tags object = {}

@description('Name of the shared Log Analytics workspace backing the Container Apps environment.')
param logAnalyticsName string

@description('Storage account holding the Azure Files share with certificates and config.')
param storageAccountName string

@description('Azure Files share holding fleet-telemetry config, TLS cert/key, and the Tesla fleet private key.')
param configShareName string = 'telemetry-config'

@description('''
External TCP port for the telemetry endpoint. Not 443: externally exposed TCP ports must be unique
across the whole Container Apps environment, and 80/443 are already taken by the built-in HTTP
ingress that the command proxy uses. Tesla's fleet_telemetry_config takes an explicit port, so this
value just has to match what is registered on the vehicle.
''')
param telemetryPort int = 8443

@description('Container image for Tesla fleet-telemetry. Pinned rather than :latest so a vehicle-facing server never changes underneath us unannounced.')
param fleetTelemetryImage string = 'tesla/fleet-telemetry:v0.9.4'

@description('Container image for the ZMQ -> HTTP bridge, built from src/EvSolarChargeController.TelemetryBridge.')
param bridgeImage string

@description('Container image for the Tesla vehicle-command HTTP proxy.')
param teslaProxyImage string

@description('URL of the TelemetryIngest function the bridge posts to.')
param ingestUrl string

@description('Shared secret the bridge presents to the Function.')
@secure()
param ingestSharedSecret string

@description('Shared secret required on inbound calls to the command proxy.')
@secure()
param proxySharedSecret string

@description('Address space for the Container Apps VNet. A /23 leaves room for the required /27 infrastructure subnet.')
param vnetAddressPrefix string = '10.20.0.0/23'

@description('Subnet for the Container Apps environment. Must be at least /27 for workload profiles.')
param infrastructureSubnetPrefix string = '10.20.0.0/27'

var vnetName = 'vnet-${namePrefix}'
var environmentName = 'cae-${namePrefix}'
var telemetryAppName = 'ca-${namePrefix}-telemetry'
var proxyAppName = 'ca-${namePrefix}-teslaproxy'
var configVolumeName = 'config'

// Keys are read through `existing` references so they are never passed in as parameters or
// surfaced as outputs, either of which would persist them in deployment history.
resource logAnalytics 'Microsoft.OperationalInsights/workspaces@2023-09-01' existing = {
  name: logAnalyticsName
}

resource storageAccount 'Microsoft.Storage/storageAccounts@2023-05-01' existing = {
  name: storageAccountName
}

resource vnet 'Microsoft.Network/virtualNetworks@2023-11-01' = {
  name: vnetName
  location: location
  tags: tags
  properties: {
    addressSpace: {
      addressPrefixes: [
        vnetAddressPrefix
      ]
    }
    subnets: [
      {
        name: 'infrastructure'
        properties: {
          addressPrefix: infrastructureSubnetPrefix
          delegations: [
            {
              name: 'containerapps'
              properties: {
                serviceName: 'Microsoft.App/environments'
              }
            }
          ]
        }
      }
    ]
  }
}

resource managedEnvironment 'Microsoft.App/managedEnvironments@2024-03-01' = {
  name: environmentName
  location: location
  tags: tags
  properties: {
    appLogsConfiguration: {
      destination: 'log-analytics'
      logAnalyticsConfiguration: {
        customerId: logAnalytics.properties.customerId
        sharedKey: logAnalytics.listKeys().primarySharedKey
      }
    }
    vnetConfiguration: {
      // A custom VNet is a hard requirement for TCP ingress, which in turn is what lets Tesla's
      // mTLS handshake reach the fleet-telemetry process untouched.
      infrastructureSubnetId: vnet.properties.subnets[0].id
      internal: false
    }
    workloadProfiles: [
      {
        name: 'Consumption'
        workloadProfileType: 'Consumption'
      }
    ]
    zoneRedundant: false
  }
}

// Certificates and config are seeded into this share out-of-band; see docs/SETUP.md. Keeping them
// off the template means no private key is ever written into deployment history.
resource configStorage 'Microsoft.App/managedEnvironments/storages@2024-03-01' = {
  parent: managedEnvironment
  name: configVolumeName
  properties: {
    azureFile: {
      accountName: storageAccountName
      accountKey: storageAccount.listKeys().keys[0].value
      shareName: configShareName
      accessMode: 'ReadOnly'
    }
  }
}

resource telemetryApp 'Microsoft.App/containerApps@2024-03-01' = {
  name: telemetryAppName
  location: location
  tags: tags
  properties: {
    managedEnvironmentId: managedEnvironment.id
    workloadProfileName: 'Consumption'
    configuration: {
      activeRevisionsMode: 'Single'
      ingress: {
        external: true
        // TCP, not HTTP: the managed HTTP front end would terminate TLS and strip the client
        // certificate that fleet-telemetry needs to identify the vehicle. This is also why the
        // container serves its own Let's Encrypt certificate — Azure's free managed certificates
        // only apply to HTTP ingress.
        transport: 'tcp'
        targetPort: telemetryPort
        exposedPort: telemetryPort
        allowInsecure: false
      }
      secrets: [
        {
          name: 'ingest-shared-secret'
          value: ingestSharedSecret
        }
      ]
    }
    template: {
      containers: [
        {
          name: 'fleet-telemetry'
          image: fleetTelemetryImage
          // No command/args override. The image already runs
          //   ["/fleet-telemetry", "-config", "/etc/fleet-telemetry/config.json"]
          // and Container Apps' `args` replaces the command rather than appending to it, so an
          // override here gets exec'd as the binary itself.
          // Platform minimum. This replica bills 24/7 and the free grant covers only ~0.069 vCPU
          // continuously, so every fraction above the floor is money. One or two vehicles sending
          // a handful of signals a minute does not need more.
          resources: {
            cpu: json('0.25')
            memory: '0.5Gi'
          }
          volumeMounts: [
            {
              volumeName: configVolumeName
              mountPath: '/etc/fleet-telemetry'
            }
          ]
        }
        {
          name: 'bridge'
          image: bridgeImage
          env: [
            {
              name: 'BRIDGE__ZmqEndpoint'
              // Containers in one app share a network namespace, so the dispatcher socket never
              // leaves localhost.
              value: 'tcp://127.0.0.1:5284'
            }
            {
              name: 'BRIDGE__IngestUrl'
              value: ingestUrl
            }
            {
              name: 'BRIDGE__IngestSharedSecret'
              secretRef: 'ingest-shared-secret'
            }
          ]
          resources: {
            cpu: json('0.25')
            memory: '0.5Gi'
          }
        }
      ]
      volumes: [
        {
          name: configVolumeName
          storageType: 'AzureFile'
          storageName: configVolumeName
        }
      ]
      scale: {
        // Vehicles hold a long-lived connection, so one replica must always be up; more than one
        // would split connections across replicas with no shared state.
        minReplicas: 1
        maxReplicas: 1
      }
    }
  }
  dependsOn: [
    configStorage
  ]
}

resource proxyApp 'Microsoft.App/containerApps@2024-03-01' = {
  name: proxyAppName
  location: location
  tags: tags
  properties: {
    managedEnvironmentId: managedEnvironment.id
    workloadProfileName: 'Consumption'
    configuration: {
      activeRevisionsMode: 'Single'
      ingress: {
        external: true
        transport: 'http'
        // The image fronts tesla-http-proxy with a local reverse proxy: Container Apps terminates
        // TLS and forwards plain HTTP here, which is then re-wrapped for the proxy's mandatory TLS.
        targetPort: 8080
        allowInsecure: false
      }
      secrets: [
        {
          name: 'proxy-shared-secret'
          value: proxySharedSecret
        }
      ]
    }
    template: {
      containers: [
        {
          name: 'tesla-http-proxy'
          image: teslaProxyImage
          env: [
            {
              name: 'TESLA_KEY_FILE'
              value: '/etc/tesla/fleet-key.pem'
            }
            {
              name: 'TESLA_HTTP_PROXY_TLS_CERT'
              value: '/etc/tesla/proxy-cert.pem'
            }
            {
              name: 'TESLA_HTTP_PROXY_TLS_KEY'
              value: '/etc/tesla/proxy-key.pem'
            }
            {
              name: 'TESLA_HTTP_PROXY_PORT'
              value: '4443'
            }
            {
              name: 'PROXY_SHARED_SECRET'
              secretRef: 'proxy-shared-secret'
            }
          ]
          resources: {
            cpu: json('0.25')
            memory: '0.5Gi'
          }
          volumeMounts: [
            {
              volumeName: configVolumeName
              mountPath: '/etc/tesla'
            }
          ]
        }
      ]
      volumes: [
        {
          name: configVolumeName
          storageType: 'AzureFile'
          storageName: configVolumeName
        }
      ]
      scale: {
        // Commands are rare (a handful a day), so scaling to zero between them is free.
        minReplicas: 0
        maxReplicas: 1
      }
    }
  }
  dependsOn: [
    configStorage
  ]
}

output telemetryFqdn string = telemetryApp.properties.configuration.ingress.fqdn
output telemetryPort int = telemetryPort
output proxyFqdn string = proxyApp.properties.configuration.ingress.fqdn
output proxyBaseUrl string = 'https://${proxyApp.properties.configuration.ingress.fqdn}'
output environmentId string = managedEnvironment.id

@description('Point the DuckDNS A record for the telemetry hostname at this address.')
output environmentStaticIp string = managedEnvironment.properties.staticIp
