export namespace app {
	
	export class FileEntry {
	    name: string;
	    size: number;
	    lastModified: time.Time;
	    loaded: boolean;
	
	    static createFrom(source: any = {}) {
	        return new FileEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.size = source["size"];
	        this.lastModified = this.convertValues(source["lastModified"], time.Time);
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
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new LoadSummary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.rowsInserted = source["rowsInserted"];
	        this.linesSkipped = source["linesSkipped"];
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

export namespace store {
	
	export class Filters {
	    timeFrom?: time.Time;
	    timeTo?: time.Time;
	    level: string;
	    msg: string;
	    topic: string;
	    accession: string;
	    studyUid: string;
	    text: string;
	
	    static createFrom(source: any = {}) {
	        return new Filters(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.timeFrom = this.convertValues(source["timeFrom"], time.Time);
	        this.timeTo = this.convertValues(source["timeTo"], time.Time);
	        this.level = source["level"];
	        this.msg = source["msg"];
	        this.topic = source["topic"];
	        this.accession = source["accession"];
	        this.studyUid = source["studyUid"];
	        this.text = source["text"];
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
	export class LogRow {
	    fileHash: string;
	    effectiveTs?: time.Time;
	    level?: string;
	    msg?: string;
	    topic?: string;
	    accession?: string;
	    studyUid?: string;
	    sourceFile: string;
	    sourceLine: number;
	
	    static createFrom(source: any = {}) {
	        return new LogRow(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.fileHash = source["fileHash"];
	        this.effectiveTs = this.convertValues(source["effectiveTs"], time.Time);
	        this.level = source["level"];
	        this.msg = source["msg"];
	        this.topic = source["topic"];
	        this.accession = source["accession"];
	        this.studyUid = source["studyUid"];
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
	    truncated: boolean;
	
	    static createFrom(source: any = {}) {
	        return new SearchResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.rows = this.convertValues(source["rows"], LogRow);
	        this.truncated = source["truncated"];
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

export namespace time {
	
	export class Time {
	
	
	    static createFrom(source: any = {}) {
	        return new Time(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	
	    }
	}

}

