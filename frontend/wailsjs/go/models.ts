export namespace app {
	
	export class FileEntry {
	    name: string;
	    size: number;
	    // Go type: time
	    lastModified: any;
	    loaded: boolean;
	
	    static createFrom(source: any = {}) {
	        return new FileEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.size = source["size"];
	        this.lastModified = this.convertValues(source["lastModified"], null);
	        this.loaded = source["loaded"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class LoadSummary {
	    name: string;
	    rowsInserted: number;
	    linesSkipped: number;
	    alreadyLoaded?: boolean;
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new LoadSummary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.rowsInserted = source["rowsInserted"];
	        this.linesSkipped = source["linesSkipped"];
	        this.alreadyLoaded = source["alreadyLoaded"];
	        this.error = source["error"];
	    }
	}

}

export namespace gcp {
	
	export class AuthStatus {
	    available: boolean;
	    account: string;
	    projectId: string;
	    message: string;
	
	    static createFrom(source: any = {}) {
	        return new AuthStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.available = source["available"];
	        this.account = source["account"];
	        this.projectId = source["projectId"];
	        this.message = source["message"];
	    }
	}
	export class Project {
	    id: string;
	    name: string;
	
	    static createFrom(source: any = {}) {
	        return new Project(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	    }
	}

}

export namespace gcs {
	
	export class BucketInfo {
	    name: string;
	
	    static createFrom(source: any = {}) {
	        return new BucketInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	    }
	}

}

export namespace parse {
	
	export class Field {
	    column: string;
	    jsonKeys: string[];
	    required: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Field(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.column = source["column"];
	        this.jsonKeys = source["jsonKeys"];
	        this.required = source["required"];
	    }
	}

}

export namespace store {
	
	export class Filters {
	    // Go type: time
	    timeFrom?: any;
	    // Go type: time
	    timeTo?: any;
	    level: string;
	    fields: Record<string, string>;
	    text: string;
	    offset: number;
	
	    static createFrom(source: any = {}) {
	        return new Filters(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.timeFrom = this.convertValues(source["timeFrom"], null);
	        this.timeTo = this.convertValues(source["timeTo"], null);
	        this.level = source["level"];
	        this.fields = source["fields"];
	        this.text = source["text"];
	        this.offset = source["offset"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class GCSSourceConfig {
	    projectId: string;
	    bucket: string;
	
	    static createFrom(source: any = {}) {
	        return new GCSSourceConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.projectId = source["projectId"];
	        this.bucket = source["bucket"];
	    }
	}
	export class LogRow {
	    fileHash: string;
	    // Go type: time
	    time?: any;
	    level?: string;
	    fields: Record<string, string>;
	    sourceFile: string;
	    sourceLine: number;
	
	    static createFrom(source: any = {}) {
	        return new LogRow(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.fileHash = source["fileHash"];
	        this.time = this.convertValues(source["time"], null);
	        this.level = source["level"];
	        this.fields = source["fields"];
	        this.sourceFile = source["sourceFile"];
	        this.sourceLine = source["sourceLine"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SearchResult {
	    rows: LogRow[];
	    hasMore: boolean;
	
	    static createFrom(source: any = {}) {
	        return new SearchResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.rows = this.convertValues(source["rows"], LogRow);
	        this.hasMore = source["hasMore"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class Wiretap {
	    id: string;
	    name: string;
	    sourceType: string;
	    gcs?: GCSSourceConfig;
	    prefix: string;
	    tableName: string;
	    fields: parse.Field[];
	    retentionDays: number;
	    autoLoadEnabled: boolean;
	    pollIntervalMinutes: number;
	    // Go type: time
	    createdAt: any;
	    // Go type: time
	    lastPolledAt?: any;
	
	    static createFrom(source: any = {}) {
	        return new Wiretap(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.sourceType = source["sourceType"];
	        this.gcs = this.convertValues(source["gcs"], GCSSourceConfig);
	        this.prefix = source["prefix"];
	        this.tableName = source["tableName"];
	        this.fields = this.convertValues(source["fields"], parse.Field);
	        this.retentionDays = source["retentionDays"];
	        this.autoLoadEnabled = source["autoLoadEnabled"];
	        this.pollIntervalMinutes = source["pollIntervalMinutes"];
	        this.createdAt = this.convertValues(source["createdAt"], null);
	        this.lastPolledAt = this.convertValues(source["lastPolledAt"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class WiretapInput {
	    name: string;
	    sourceType: string;
	    gcs?: GCSSourceConfig;
	    prefix: string;
	    fields: parse.Field[];
	    retentionDays: number;
	    autoLoadEnabled: boolean;
	    pollIntervalMinutes: number;
	
	    static createFrom(source: any = {}) {
	        return new WiretapInput(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.sourceType = source["sourceType"];
	        this.gcs = this.convertValues(source["gcs"], GCSSourceConfig);
	        this.prefix = source["prefix"];
	        this.fields = this.convertValues(source["fields"], parse.Field);
	        this.retentionDays = source["retentionDays"];
	        this.autoLoadEnabled = source["autoLoadEnabled"];
	        this.pollIntervalMinutes = source["pollIntervalMinutes"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

