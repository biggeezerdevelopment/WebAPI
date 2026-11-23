# WebAPI Agent

A Go-based agent that collects and forwards metrics from various web APIs to a centralized logging system.

## Overview

This project implements a configurable agent that can:
- Monitor multiple web APIs simultaneously
- Collect metrics from configured endpoints
- Forward collected data to a remote logging system
- Support various authentication methods
- Handle different API request methods and content types

## Prerequisites

- Go 1.23.1 or later
- Git

## Installation

1. Clone the repository:
```bash
git clone <repository-url>
cd WebAPI
```

2. Install dependencies:
```bash
go mod download
```

## Configuration

The project uses a YAML configuration file (`config.yml`) with the following structure:

### JarvisAgent Configuration
```yaml
jarvisagent:
    name: "jarvis-agent-windows"  # Name of the agent
    id: "1234567890"             # Unique identifier for the agent
```

### API Log Remote Configuration
```yaml
apilogremote:
    enabled: false               # Enable/disable remote logging
    api_url: "http://localhost:8080/api/v1/logs"
    api_method: "POST"
    skip_verify: false           # Skip TLS certificate verification (default: false)
    api_headers:
        - key: "Content-Type"
          value: "application/json"
```

TLS certificates from the remote log server are verified by default. Set `skip_verify: true` only for a trusted server with a self-signed or otherwise unverifiable certificate, because it leaves log traffic and API headers open to interception.

### WebAPI Configuration
```yaml
webapi:
    - enabled: false            # Enable/disable this API
      name: "qualys"            # Name of the API
      subject: "QualysAPI"      # Subject identifier
      endpoint: "http://localhost:8080/api/v1/metrics"
      method: "post"
      headers:
        - key: "Content-Type"
          value: "application/json"
      auth_type: "apikey"       # Authentication type
      api_key: "1234567890"     # API key for authentication
      skip_verify: true         # Skip SSL verification
      sleeptime: 30            # Time between requests in seconds
      timeout: 30              # Request timeout in seconds
      params:
        metrics: "20"          # Query parameters
      body:
        - "metrics"            # Request body parameters
```

## Building

To build the project:

```bash
go build -o webapi-agent
```

## Running

1. Configure the `config.yml` file according to your needs
2. Run the agent:
```bash
./webapi-agent
```

## Features

- Multiple API monitoring support
- Configurable request intervals
- Remote logging capability
- Support for various authentication methods:
  - API Key
  - Basic Authentication
- Customizable headers and request parameters
- SSL verification options
- Configurable timeouts

## Project Structure

```
.
├── api/            # API client implementations
├── config/         # Configuration handling
├── main.go         # Main application entry point
├── config.yml      # Configuration file
├── go.mod          # Go module definition
└── go.sum          # Go module checksums
```

## Error Handling

The agent includes comprehensive error handling:
- Failed API requests are logged
- Remote logging failures are logged
- Configuration loading errors are handled gracefully

## Contributing

1. Fork the repository
2. Create your feature branch
3. Commit your changes
4. Push to the branch
5. Create a new Pull Request

## License

[Add your license information here] 