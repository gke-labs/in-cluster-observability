# OpenTelemetry Sink
This is a simple OpenTelemetry receiver that writes OTLP data (traces, metrics, and logs) to a local binary file.

## File Format
The file format follows the kOps OpenTelemetry trace file format. Each object is written with a 16-byte header followed by the marshaled protobuf data.

### Header (16 bytes, big-endian)
- Bytes 0-3: Data length (uint32)
- Bytes 4-7: CRC32 checksum (IEEE) of the data (uint32)
- Bytes 8-11: Flags (currently unused, set to 0) (uint32)
- Bytes 12-15: TypeCode (uint32)

### Object Types
- `TypeCode 1`: `ObjectType` message, used to map a `TypeCode` to a protobuf type name.
- `TypeCode >= 32`: User-defined types (e.g., `ExportTraceServiceRequest`).

The `ObjectType` message itself is a protobuf message with the following structure:
- Field 1 (varint): `type_code`
- Field 2 (string): `type_name`

## Usage
```bash
go run ./cmd/opentelemetry-sink --addr :4317 --path otel-data.bin
```

## Configuration
- `--addr`: Address to listen on (default: `:4317`).
- `--path`: Path to the output file (default: `otel-data.bin`).
