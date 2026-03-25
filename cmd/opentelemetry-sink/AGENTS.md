# OpenTelemetry Sink Design Details

The OpenTelemetry Sink is designed as a simple, high-performance receiver that can be embedded or run as a standalone sidecar.

## Design Decisions

- **Local Storage**: Data is written directly to a local file to avoid memory overhead associated with buffering. This is useful for high-volume data collection on a single node.
- **kOps-Compatible Format**: Using the same binary format as kOps' `otlptracefile` allows for potential reuse of existing tooling.
- **gRPC-only Support**: At the moment, only the gRPC protocol is supported for OTLP data.
- **Minimal Dependencies**: The binary is kept as small as possible by only including necessary protobuf and gRPC libraries.

## Future Considerations

- **Rotation**: Implementing file rotation to prevent the sink from filling up the disk.
- **Metrics-Server Protocol**: Adding support for querying the stored metrics using the metrics-server protocol for HPA integration.
- **HTTP Support**: Implementing OTLP-over-HTTP if needed.
