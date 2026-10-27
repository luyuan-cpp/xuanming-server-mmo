package com.game.authority;

import java.sql.*;
import java.util.*;

/** 完整扫描后才按受支持的数据语句族接受；不改写SQL，不用startsWith放行。 */
public final class AuthoritySql {
    enum Kind { READ, WRITE, DEADLINE }
    private enum TokenKind { WORD, QUOTED_IDENTIFIER, VALUE, PUNCTUATION }
    private record Token(TokenKind kind, String text) {}
    private static final Set<String> CONTROL = Set.of("CALL","CREATE","ALTER","DROP","TRUNCATE","RENAME",
            "GRANT","REVOKE","BEGIN","COMMIT","ROLLBACK","SAVEPOINT","RELEASE","START","UNLOCK",
            "LOAD","OUTFILE","DUMPFILE","USE","PREPARE","EXECUTE","DEALLOCATE","SHOW","DESCRIBE",
            "EXPLAIN","ANALYZE","OPTIMIZE","REPAIR","FLUSH","RESET","KILL","SHUTDOWN",
            "BACKUP","RESTORE","GET_LOCK","RELEASE_LOCK","IS_USED_LOCK","SLEEP","BENCHMARK");
    private static final Set<String> PROTECTED = Set.of("ROUTE_META","WRITER_REGISTRY","WRITER_SLOT",
            "WRITER_LIFECYCLE_RECEIPT","SQL_WRITER_DRAIN_RUN","SQL_WRITER_FREEZE_TICKET",
            "SQL_WRITER_MAINTENANCE_RECEIPT");
    private AuthoritySql() {}
    static Kind validate(String sql) throws SQLException {
        List<Token> tokens = scan(sql);
        if (tokens.isEmpty()) throw denied("空SQL");
        if (word(tokens,0,"SET")) { deadline(tokens); return Kind.DEADLINE; }
        int command = 0;
        if (word(tokens,0,"WITH")) {
            int depth = 0; command = -1;
            for (int i=1;i<tokens.size();i++) {
                String value=tokens.get(i).text();
                if (value.equals("(")) depth++;
                else if (value.equals(")")) depth--;
                else if(depth==0 && tokens.get(i).kind()==TokenKind.WORD
                        && Set.of("SELECT","INSERT","UPDATE","DELETE","REPLACE").contains(value)) { command=i; break; }
            }
            if(command<0)throw denied("WITH缺少完整数据主体");
        }
        String family=tokens.get(command).text();
        if(tokens.get(command).kind()!=TokenKind.WORD
                || !Set.of("SELECT","INSERT","UPDATE","DELETE","REPLACE").contains(family)) throw denied("不支持的数据语句族");
        if(command+1>=tokens.size())throw denied("不完整的数据语句");
        boolean mutation=!family.equals("SELECT"), locking=false, protectedReference=false;
        for(int i=0;i<tokens.size();i++) {
            Token token=tokens.get(i);
            if(token.kind()==TokenKind.WORD && (CONTROL.contains(token.text())||token.text().equals("SET")
                    && !family.equals("UPDATE")&&!family.equals("INSERT")))throw denied("SQL包含控制、DDL或外部副作用");
            if(token.kind()==TokenKind.WORD && token.text().equals("LOCK")
                    && !(word(tokens,i+1,"IN")&&word(tokens,i+2,"SHARE")&&word(tokens,i+3,"MODE")))
                throw denied("非数据锁语法");
            if(token.kind()==TokenKind.WORD && (token.text().equals("FOR")
                    &&(word(tokens,i+1,"UPDATE")||word(tokens,i+1,"SHARE"))||token.text().equals("LOCK")))locking=true;
            if((token.kind()==TokenKind.WORD||token.kind()==TokenKind.QUOTED_IDENTIFIER)
                    && PROTECTED.contains(token.text().toUpperCase(Locale.ROOT)))protectedReference=true;
            if(token.text().equals(":="))throw denied("会话赋值");
        }
        if(protectedReference && (mutation||locking))throw denied("normal不得修改或锁定authority控制表");
        if(family.equals("INSERT")||family.equals("REPLACE")) {
            int at=command+1;
            while(at<tokens.size()&&Set.of("LOW_PRIORITY","DELAYED","HIGH_PRIORITY","IGNORE").contains(tokens.get(at).text()))at++;
            if(word(tokens,at,"INTO"))at++;
            identifier(tokens,at);
            if(tokens.stream().noneMatch(t->Set.of("VALUES","VALUE","SELECT","SET","DEFAULT").contains(t.text())))
                throw denied("INSERT/REPLACE缺少数据源");
        } else if(family.equals("UPDATE")) {
            identifier(tokens,command+1);
            if(tokens.stream().noneMatch(t->t.kind()==TokenKind.WORD&&t.text().equals("SET")))throw denied("UPDATE缺少SET");
        } else if(family.equals("DELETE")) {
            if(tokens.stream().noneMatch(t->t.kind()==TokenKind.WORD&&t.text().equals("FROM")))throw denied("DELETE缺少FROM");
        } else if(tokens.stream().anyMatch(t->t.kind()==TokenKind.WORD&&t.text().equals("INTO")))
            throw denied("SELECT不允许输出或赋值目的地");
        return family.equals("SELECT")?Kind.READ:Kind.WRITE;
    }
    private static void identifier(List<Token> t,int i)throws SQLException {
        if(i>=t.size() || t.get(i).kind()!=TokenKind.WORD && t.get(i).kind()!=TokenKind.QUOTED_IDENTIFIER)
            throw denied("缺少表标识符");
    }
    static long[] deadlineCaps(String sql)throws SQLException {
        List<Token> tokens=scan(sql);deadline(tokens);return new long[]{Long.parseLong(tokens.get(4).text()),Long.parseLong(tokens.get(8).text())};
    }
    private static void deadline(List<Token> t)throws SQLException {
        if(t.size()!=9||!word(t,1,"SESSION")||!word(t,2,"MAX_EXECUTION_TIME")||!t.get(3).text().equals("=")
                || !t.get(5).text().equals(",")||!word(t,6,"TIDB_DML_MAX_EXECUTION_TIME")
                ||!t.get(7).text().equals("="))throw denied("仅允许原StrongPolicy精确期限SET");
        for(int i:new int[]{4,8}) {
            String number=t.get(i).text();
            if(t.get(i).kind()!=TokenKind.VALUE||!number.matches("[0-9]+"))throw denied("期限必须为非负整数");
            try{if(Long.parseLong(number)>Integer.MAX_VALUE)throw denied("期限超出原JDBC范围");}
            catch(NumberFormatException invalid){throw denied("期限溢出");}
        }
    }
    private static boolean word(List<Token> t,int i,String value) {
        return i>=0&&i<t.size()&&t.get(i).kind()==TokenKind.WORD&&t.get(i).text().equals(value);
    }
    private static List<Token> scan(String sql)throws SQLException {
        if(sql==null||sql.indexOf('\0')>=0)throw denied("SQL为空或含NUL");
        List<Token> tokens=new ArrayList<>();int depth=0;boolean ended=false;
        for(int i=0;i<sql.length();) {
            char c=sql.charAt(i);
            if(Character.isWhitespace(c)){i++;continue;}
            if(c=='#'||c=='-'&&i+2<sql.length()&&sql.charAt(i+1)=='-'&&Character.isWhitespace(sql.charAt(i+2))) {
                while(i<sql.length()&&sql.charAt(i)!='\n'&&sql.charAt(i)!='\r')i++;continue;
            }
            if(c=='/'&&i+1<sql.length()&&sql.charAt(i+1)=='*') {
                int close=sql.indexOf("*/",i+2);if(close<0)throw denied("未闭合注释");
                String body=sql.substring(i+2,close).stripLeading();
                if(body.startsWith("!")||body.startsWith("+")||body.toUpperCase(Locale.ROOT).startsWith("T!"))
                    throw denied("拒绝可执行注释与会话提示");
                i=close+2;continue;
            }
            if(ended)throw denied("禁止多语句");
            if(c==';'){ended=true;i++;continue;}
            if(c=='\''||c=='"'||c==96) {
                char quote=c;StringBuilder literal=new StringBuilder();boolean closed=false;i++;
                while(i<sql.length()) {
                    char value=sql.charAt(i++);
                    if(value=='\\'){if(i==sql.length())throw denied("未闭合转义");literal.append(value).append(sql.charAt(i++));continue;}
                    if(value==quote) {
                        if(i<sql.length()&&sql.charAt(i)==quote){literal.append(quote);i++;continue;}
                        closed=true;break;
                    }
                    literal.append(value);
                }
                if(!closed)throw denied("未闭合引号");
                tokens.add(new Token(quote==96?TokenKind.QUOTED_IDENTIFIER:TokenKind.VALUE,literal.toString()));continue;
            }
            if(Character.isLetter(c)||c=='_'||c=='$') {
                int start=i++;
                while(i<sql.length()&&(Character.isLetterOrDigit(sql.charAt(i))||sql.charAt(i)=='_'||sql.charAt(i)=='$'))i++;
                tokens.add(new Token(TokenKind.WORD,sql.substring(start,i).toUpperCase(Locale.ROOT)));continue;
            }
            if(Character.isDigit(c)) {
                int start=i++;
                while(i<sql.length()&&(Character.isLetterOrDigit(sql.charAt(i))||sql.charAt(i)=='.'||sql.charAt(i)=='_'))i++;
                tokens.add(new Token(TokenKind.VALUE,sql.substring(start,i)));continue;
            }
            if(c=='(')depth++;else if(c==')'&&--depth<0)throw denied("括号失配");
            if(c=='{'||c=='}')throw denied("不接受JDBC控制转义");
            String symbol=String.valueOf(c);
            if(c==':'&&i+1<sql.length()&&sql.charAt(i+1)=='='){symbol=":=";i++;}
            tokens.add(new Token(TokenKind.PUNCTUATION,symbol));i++;
        }
        if(depth!=0)throw denied("括号失配");
        return List.copyOf(tokens);
    }
    private static SQLException denied(String reason){return new SQLSyntaxErrorException(reason,"42000");}
}

