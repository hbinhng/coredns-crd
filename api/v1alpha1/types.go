package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type DNSSlice struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DNSSliceSpec   `json:"spec,omitempty"`
	Status DNSSliceStatus `json:"status,omitempty"`
}

type DNSSliceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []DNSSlice `json:"items"`
}

type DNSSliceSpec struct {
	DefaultTTL *int32     `json:"defaultTTL,omitempty"`
	Entries    []DNSEntry `json:"entries"`
}

type DNSEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	TTL  *int32 `json:"ttl,omitempty"`

	A     *ARecord     `json:"a,omitempty"`
	AAAA  *AAAARecord  `json:"aaaa,omitempty"`
	CNAME *CNAMERecord `json:"cname,omitempty"`
	TXT   *TXTRecord   `json:"txt,omitempty"`
	SRV   *SRVRecord   `json:"srv,omitempty"`
	Raw   *RawRecord   `json:"raw,omitempty"`
}

type ARecord struct {
	Address string `json:"address"`
}

type AAAARecord struct {
	Address string `json:"address"`
}

type CNAMERecord struct {
	Target string `json:"target"`
}

type TXTRecord struct {
	Values []string `json:"values"`
}

type SRVRecord struct {
	Priority int32  `json:"priority"`
	Weight   int32  `json:"weight"`
	Port     int32  `json:"port"`
	Target   string `json:"target"`
}

type RawRecord struct {
	RData string `json:"rdata"`
}

type DNSSliceStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	ConflictedEntries  []string           `json:"conflictedEntries,omitempty"`
}
