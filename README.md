# Kademlia Relay P2P System

A robust P2P system built with libp2p implementing Kademlia DHT, protocol bridging, and advanced peer discovery mechanisms.


    ## Table of Contents
    - [Features](#features)
    - [Architecture](#architecture)
    - [Installation](#installation)
    - [Configuration](#configuration)
    - [API Documentation](#api-documentation)
    - [Usage Examples](#usage-examples)
    - [Performance Metrics](#performance-metrics)
    - [Development](#development)
    - [Testing](#testing)
    - [Deployment](#deployment)
    - [Contributing](#contributing)
    - [License](#license)



    ## Features

    ### Core Functionality
    - **Kademlia DHT Implementation**: Full implementation of the Kademlia distributed hash table
    - **Protocol Bridging**: Interoperability between different protocol versions
    - **Multi-Transport Support**: TCP, QUIC, and WebSocket transports
    - **NAT Traversal**: Automatic NAT hole punching

    ### Discovery Mechanisms
    - **mDNS Discovery**: Local network peer discovery
    - **DHT-Based Discovery**: Global peer discovery through the DHT
    - **Peer Exchange**: Direct peer information sharing
    - **Latency-Based Optimization**: Regional peer prioritization

    ### Advanced Features
    - **PubSub Integration**: GossipSub protocol implementation
    - **Connection Management**: Priority-based connection handling
    - **State Persistence**: Periodic state saving and recovery
    - **REST API**: Comprehensive HTTP interface for monitoring and control



    ## Architecture

    ```mermaid
    graph TD
        A[KademliaRelay] --> B[DiscoveryManager]
        A --> C[ProtocolBridge]
        A --> D[PubSubManager]
        A --> E[ConnectionManager]
        A --> F[StateManager]

        B --> G[mDNS Discovery]
        B --> H[DHT Discovery]
        B --> I[Peer Exchange]

        C --> J[Protocol Negotiation]
        C --> K[Stream Bridging]

        D --> L[Topic Management]
        D --> M[Message Pub/Sub]

        E --> N[Connection Scoring]
        E --> O[Priority Handling]

        F --> P[State Persistence]
        F --> Q[Backup Rotation]
    ```



    ## Installation

    ### Prerequisites
    - Go 1.18+
    - libp2p dependencies

    ### Building from Source
    ```bash
    git clone https://github.com/SelimCelen/kadserver.git
    cd krelay
    go build -o krelay .
    ```

    ### Running with Docker
    ```bash
    docker build -t krelay .
    docker run -p 5000:5000 -p 4001:4001 krelay



    ## Configuration

    The system is configured via `config.json`. Example configuration:

    ```json
    {
        "listenAddrs": [
            "/ip4/0.0.0.0/tcp/4001",
            "/ip6/::/tcp/4001",
            "/ip4/0.0.0.0/udp/4001/quic",
            "/ip6/::/udp/4001/quic"
        ],
        "bootstrapPeers": [
            "/dnsaddr/bootstrap.libp2p.io/p2p/QmNnooDu7bfjPFoTZYxMNLWUQJyrVwtbZg5gBMjTezGAJN"
        ],
        "enableRelay": true,
        "enableAutoRelay": true,
        "apiPort": 5000,
        "maxConnections": 1000,
        "pubsub": {
            "enabled": true,
            "defaultTopics": ["krelay-global"]
        }
    }
    ```

    | Parameter | Description | Default |
    |-----------|-------------|---------|
    | `listenAddrs` | Addresses to listen on | Various |
    | `bootstrapPeers` | Initial peers for bootstrapping | Public libp2p bootstrap nodes |
    | `enableRelay` | Enable circuit relay functionality | true |
    | `apiPort` | REST API port | 5000 |



    ## API Documentation

    ### DHT Endpoints

    | Endpoint | Method | Description |
    |----------|--------|-------------|
    | `/api/v1/dht/peers` | GET | List all peers in routing table |
    | `/api/v1/dht/routing` | GET | Get routing table information |
    | `/api/v1/dht/query/{key}` | GET | Find closest peers to a key |
    | `/api/v1/dht/provide/{cid}` | POST | Announce content to the network |
    | `/api/v1/dht/findprovs/{cid}` | GET | Find providers for content |
    | `/api/v1/dht/findpeer/{peerID}` | GET | Find a specific peer |
    | `/api/v1/dht/get/{key}` | GET | Get a value from DHT |
    | `/api/v1/dht/put/{key}` | POST | Store a value in DHT |
    | `/api/v1/dht/bootstrap` | POST | Trigger DHT bootstrap |

    ### System Endpoints

    | Endpoint | Method | Description |
    |----------|--------|-------------|
    | `/api/v1/info` | GET | Get node information |
    | `/api/v1/peers` | GET | List connected peers |
    | `/api/v1/config` | GET/PUT | Get/update configuration |
    | `/api/v1/pubsub/topics` | GET | List pubsub topics |
    | `/api/v1/pubsub/topics/{topic}` | POST/DELETE | Join/leave topic |



    ## Usage Examples

    ### Starting the Node
    ```bash
    ./krelay --config config.json
    ```

    ### Using the API
    ```bash
    # Get node info
    curl http://localhost:5000/api/v1/info

    # Store a value in DHT
    curl -X POST -H "Content-Type: application/json" \
        -d '{"value":"test data"}' \
        http://localhost:5000/api/v1/dht/put/test-key

    # Subscribe to pubsub topic
    curl -X POST http://localhost:5000/api/v1/pubsub/topics/test-topic/subscribe
    ```

    ### Protocol Bridging
    The system automatically bridges between protocol versions when configured:
    ```json
    "protocolCapabilities": [
        {
            "protocolId": "/chat/1.0.0",
            "version": "1.0.0",
            "priority": 1
        },
        {
            "protocolId": "/chat/2.0.0",
            "version": "2.0.0",
            "priority": 10
        }
    ]
    ```



    ## Performance Metrics

    Typical performance characteristics:

    | Metric | Value |
    |--------|-------|
    | Peer discovery time | < 2s |
    | DHT query latency | 50-200ms |
    | Connection setup time | < 1s |
    | PubSub message propagation | < 100ms (local) |

    ### Load Testing
    Use the included load test script:
    ```bash
    python load_test.py
    ```



    ## Development

    ### Building
    ```bash
    go build
    ```

   

    ### Code Structure
    ```
    /main.go         - Main application
    
    ```



    ## Contributing

    1. Fork the repository
    2. Create a feature branch (`git checkout -b feature/your-feature`)
    3. Commit your changes (`git commit -am 'Add some feature'`)
    4. Push to the branch (`git push origin feature/your-feature`)
    5. Open a Pull Request

    ### Code Style
    - Follow standard Go formatting
    - Include unit tests for new features
    - Document public APIs



    ## License

    Copyright 2025 Selim Çelen

    Licensed under the Apache License, Version 2.0 (the "License");
    you may not use this file except in compliance with the License.
    You may obtain a copy of the License at

        http://www.apache.org/licenses/LICENSE-2.0

    Unless required by applicable law or agreed to in writing, software
    distributed under the License is distributed on an "AS IS" BASIS,
    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
    See the License for the specific language governing permissions and
    limitations under the License.
