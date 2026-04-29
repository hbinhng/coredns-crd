package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func (in *DNSSlice) DeepCopyInto(out *DNSSlice) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *DNSSlice) DeepCopy() *DNSSlice {
	if in == nil {
		return nil
	}
	out := new(DNSSlice)
	in.DeepCopyInto(out)
	return out
}

func (in *DNSSlice) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *DNSSliceList) DeepCopyInto(out *DNSSliceList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]DNSSlice, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *DNSSliceList) DeepCopy() *DNSSliceList {
	if in == nil {
		return nil
	}
	out := new(DNSSliceList)
	in.DeepCopyInto(out)
	return out
}

func (in *DNSSliceList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *DNSSliceSpec) DeepCopyInto(out *DNSSliceSpec) {
	*out = *in
	if in.DefaultTTL != nil {
		out.DefaultTTL = new(int32)
		*out.DefaultTTL = *in.DefaultTTL
	}
	if in.Entries != nil {
		out.Entries = make([]DNSEntry, len(in.Entries))
		for i := range in.Entries {
			in.Entries[i].DeepCopyInto(&out.Entries[i])
		}
	}
}

func (in *DNSSliceStatus) DeepCopyInto(out *DNSSliceStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
	if in.ConflictedEntries != nil {
		out.ConflictedEntries = make([]string, len(in.ConflictedEntries))
		copy(out.ConflictedEntries, in.ConflictedEntries)
	}
}

func (in *DNSEntry) DeepCopyInto(out *DNSEntry) {
	*out = *in
	if in.TTL != nil {
		out.TTL = new(int32)
		*out.TTL = *in.TTL
	}
	if in.A != nil {
		out.A = new(ARecord)
		*out.A = *in.A
	}
	if in.AAAA != nil {
		out.AAAA = new(AAAARecord)
		*out.AAAA = *in.AAAA
	}
	if in.CNAME != nil {
		out.CNAME = new(CNAMERecord)
		*out.CNAME = *in.CNAME
	}
	if in.TXT != nil {
		out.TXT = new(TXTRecord)
		in.TXT.DeepCopyInto(out.TXT)
	}
	if in.SRV != nil {
		out.SRV = new(SRVRecord)
		*out.SRV = *in.SRV
	}
	if in.Raw != nil {
		out.Raw = new(RawRecord)
		*out.Raw = *in.Raw
	}
}

func (in *TXTRecord) DeepCopyInto(out *TXTRecord) {
	*out = *in
	if in.Values != nil {
		out.Values = make([]string, len(in.Values))
		copy(out.Values, in.Values)
	}
}
