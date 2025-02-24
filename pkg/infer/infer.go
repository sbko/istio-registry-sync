package infer

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"istio.io/api/networking/v1alpha3"
	ic "istio.io/client-go/pkg/apis/networking/v1alpha3"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServiceEntry infers an Istio service entry based on provided information
func ServiceEntry(owner v1.OwnerReference, prefix, host string, workloadEntries []*v1alpha3.WorkloadEntry) *ic.ServiceEntry {
	addresses := []string{}
	if len(workloadEntries) > 0 {
		if ip := net.ParseIP(workloadEntries[0].Address); ip != nil {
			addresses = []string{workloadEntries[0].Address}
		}
	}

	return &ic.ServiceEntry{
		TypeMeta: v1.TypeMeta{},
		ObjectMeta: v1.ObjectMeta{
			Name:            ServiceEntryName(prefix, host),
			OwnerReferences: []v1.OwnerReference{owner},
		},
		Spec: v1alpha3.ServiceEntry{
			Hosts:     []string{host},
			Addresses: addresses,
			// assume external for now
			Location:   v1alpha3.ServiceEntry_MESH_EXTERNAL,
			Resolution: Resolution(workloadEntries),
			Ports:      Ports(workloadEntries),
			Endpoints:  workloadEntries,
		},
	}
}

// WorkloadEntry creates a Workload Entry from an address and port
// It infers the port name from the port number
func WorkloadEntry(address string, port uint32) *v1alpha3.WorkloadEntry {
	return &v1alpha3.WorkloadEntry{
		Address: address,
		Ports:   map[string]uint32{Proto(port): port},
	}
}

// Proto infers the port name based on the port number
func Proto(port uint32) string {
	switch port {
	case 80:
		return "http"
	case 443:
		return "https"
	default:
		return "tcp"
	}
}

// Ports uses a slice of Service Entry workload entries to create a de-duped slice of Istio Ports
// Infering name and protocol from the port number
func Ports(workloadEntries []*v1alpha3.WorkloadEntry) []*v1alpha3.ServicePort {
	dedup := map[uint32]*v1alpha3.ServicePort{}
	for _, we := range workloadEntries {
		for _, port := range we.Ports {
			dedup[port] = &v1alpha3.ServicePort{
				Name:     Proto(port),
				Number:   uint32(port),
				Protocol: strings.ToUpper(Proto(port)),
			}
		}
	}
	res := []*v1alpha3.ServicePort{}
	for _, port := range dedup {
		res = append(res, port)
	}
	sort.Slice(res, func(i, j int) bool {
		return res[i].Number < res[j].Number
	})
	return res
}

// Resolution infers STATIC resolution if there are workload entries
// If there are no workload entries it infers DNS; otherwise will return STATIC
func Resolution(workloadEntries []*v1alpha3.WorkloadEntry) v1alpha3.ServiceEntry_Resolution {
	if len(workloadEntries) == 0 {
		return v1alpha3.ServiceEntry_DNS
	}
	for _, we := range workloadEntries {
		if addr := net.ParseIP(we.Address); addr == nil {
			return v1alpha3.ServiceEntry_DNS // is not IP so DNS
		}
	}
	return v1alpha3.ServiceEntry_STATIC
}

// ServiceEntryName returns the service entry name based on the specificed host
func ServiceEntryName(prefix, host string) string {
	return fmt.Sprintf("%s%s", prefix, host)
}
