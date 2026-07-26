export interface Capability {
  name: string;
  available: boolean;
  detail?: string;
}

export interface Account {
  id: string;
  name: string;
  cloudflare_account_id: string;
  enabled: boolean;
  health_status: string;
  health_error?: string;
  capabilities?: Capability[];
}

export interface R2Object {
  key: string;
  size: number;
  etag: string;
  content_type: string;
  last_modified: string;
  state: string;
  physical_bucket_id: string;
}

export interface Bucket {
  id: string;
  account_id: string;
  name: string;
  health_status: string;
  storage_bytes: number;
  class_a_ops: number;
  class_b_ops: number;
}

export interface Credential {
  id: string;
  kind: "s3" | "webdav" | "ai";
  name: string;
  public_id: string;
  scopes: string[];
  disabled: boolean;
  secret?: string;
}
