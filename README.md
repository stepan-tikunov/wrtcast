# LMDB

This repository contains three independent applications:

- [`castctl/`](castctl/) — minimal Chromecast controller with OpenWrt build files;
- [`title-api/`](title-api/) — Go gRPC service that resolves title IDs to HLS streams;
- [`bff/`](bff/) — ucode CGI on the router that connects title-api and castctl.

Build each application from its own directory.
