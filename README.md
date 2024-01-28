# kubectl sticky-port-forward

Usually when a pod goes down for some reasons, the port-forward session will fail right after when it receives a new traffic, regardless if we restart the pod or not. You are then forced to restart the port-forward session manually.

This is a simple kubectl plugin to keep the port-forward "alive" in that scenerio. Under the hood it simply use Kubernetes API to check the pods status and restart the port-forward session automatically if necessary. It also supports port-forward multiple ports at once.

Disclaimer: This is just a toy project for me to play around with Go and Kubernetes API. In reality, you can just set up an Ingress instead.

## Features
- Automatically restart the port-forward session when the pods are stopped for any reason.
- Can use a yaml file as configuration to avoid typing all the params every time.
- Can be used to port-forward multiple ports at once when using yaml file.

## Build
At the root directory of the project:
```
go build ./cmd/kubectl-sticky_port_forward.go 
```

It will output a file called `kubectl-sticky_port_forward` in the same directory.

## Install
According to [kubernetes doc](https://kubernetes.io/docs/tasks/extend-kubectl/kubectl-plugins/#discovering-plugins), you simply need to put the built binary in any directory that is in your `PATH`. Then you can call `kubectl sticky-port-forward` to use the plugin.

You can totally rename the binary to something else too. For example, renaming it to `kubectl-spf` so you can call it as `kubectl spf` which is much more convenience.

## Usage
You can start by using `kubectl sticky-port-forward` for the general help.

Example for inline usage:
```
# port-forward based on label
kubectl sticky-port-forward --labels app=appname 80:8080

# port-forward based on label with multiple ports
kubectl sticky-port-forward --labels app=appname 80:8080 443:443

# port-forward based on label with specific namespace
kubectl sticky-port-forward --labels app=appname -n othernamespace 80:8080

# port-forward based on pod name with regex matching
kubectl sticky-port-forward --pod podname.* 80:8080
```

Example with yaml config file:
```
# use a yaml file to start port-forward
kubectl sticky-port-forward -f ./config.yml

# create a new yaml config file
kubectl sticky-port-forward -l app=subscription-management-service-db 8081:8080 -o ./config.yml

# append to an existing yaml config file
kubectl sticky-port-forward -l app=subscription-management-service-db 8081:8080 -a ./config.yml
```
