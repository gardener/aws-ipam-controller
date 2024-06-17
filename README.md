# aws-ipam-controller

[![reuse compliant](https://reuse.software/badge/reuse-compliant.svg)](https://reuse.software/)

This README provides an overview of the IPAM controller for Kubernetes, designed specifically for the AWS platform. The IPAM controller is responsible for assigning pod addresses to Kubernetes node objects.

The controller operates in three modes:`ipv4`, `ipv6` and `dual-stack`. In `ipv4` mode it manages the IPv4 CIDR range and assigns addresses from this range to the Kubernetes nodes. In `ipv6` mode, it assigns IPv6 addresses to Kubernetes node objects using prefix delegation. In `dual-stack` mode, it manages the IPv4 CIDR range and assigns addresses from this range to the Kubernetes nodes, while also assigning IPv6 addresses based on prefix delegation. 

A sample configuration of the IPAM controller is shown below.

```
aws-ipam-controller
--control-kubeconfig=inClusterConfig
--cluster-name=shoot-foo
--health-probe-port=10259
--metrics-port=10258
--namespace=shoot--garden--shoot-foo
--region=eu-west-1
--target-kubeconfig=/var/run/secrets/gardener.cloud/shoot/generic-kubeconfig/kubeconfig
--leader-election=true
--leader-election-namespace=kube-system
--cluster-cidrs=10.0.0.0/16
--mode=dual-stack
--node-cidr-mask-size-ipv6=77
--tick-period=500ms
--v=4
```